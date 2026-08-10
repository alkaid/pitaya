//go:build benchmark

// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package benchmark

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/client"
	pitayamessage "github.com/alkaid/pitaya/v2/conn/message"
)

const (
	connectorRawRoute     = "connector.testsvc.testrequestreceivereturnsraw"
	connectorPtrRoute     = "connector.testsvc.testrequestreturnsptr"
	connectorSessionRoute = "connector.testsvc.testrequestonlysessionreturnsptr"
	gameSessionRoute      = "game.testsvc.testrequestonlysessionreturnsptr"
	benchmarkRequestWait  = 10 * time.Second
	// client.New currently admits at most 30 requests before SendRequest blocks.
	benchmarkMaxInflight = 30
)

var (
	host           = flag.String("host", "localhost", "server host")
	port           = flag.Int("port", 32222, "server port")
	readinessProbe = flag.Bool("readiness-probe", false, "run the server readiness probe")
	addr           string
)

type benchmarkRequestResult struct {
	response *pitayamessage.Message
	err      error
}

type benchmarkPendingRequest struct {
	result chan benchmarkRequestResult
}

type benchmarkClient struct {
	*client.Client

	stateMu     sync.Mutex
	pending     map[uint]*benchmarkPendingRequest
	early       map[uint]*pitayamessage.Message
	registering int
	failure     error

	inflight    chan struct{}
	done        chan struct{}
	failed      chan struct{}
	closeOnce   sync.Once
	requestWait time.Duration
	sendRequest func(string, []byte) (uint, error)
}

func newBenchmarkClient() *benchmarkClient {
	base := client.New()
	c := &benchmarkClient{
		Client:      base,
		pending:     make(map[uint]*benchmarkPendingRequest),
		early:       make(map[uint]*pitayamessage.Message),
		inflight:    make(chan struct{}, benchmarkMaxInflight),
		done:        make(chan struct{}),
		failed:      make(chan struct{}),
		requestWait: benchmarkRequestWait,
		sendRequest: base.SendRequest,
	}
	base.OnDisconnected(c.handleDisconnect)
	return c
}

func (c *benchmarkClient) connect(address string) error {
	if err := c.ConnectTo(address); err != nil {
		return err
	}
	go c.dispatchResponses()
	return nil
}

func (c *benchmarkClient) dispatchResponses() {
	for {
		select {
		case <-c.done:
			return
		case <-c.failed:
			return
		case response, ok := <-c.IncomingMsgChan:
			if !ok {
				c.fail(errors.New("incoming message channel closed"))
				return
			}
			if response == nil {
				c.fail(errors.New("received nil response"))
				return
			}
			if !c.handleResponse(response) {
				return
			}
		}
	}
}

func (c *benchmarkClient) handleResponse(response *pitayamessage.Message) bool {
	if response == nil {
		c.fail(errors.New("received nil response"))
		return false
	}

	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.failure != nil {
		return false
	}
	if pending, found := c.pending[response.ID]; found {
		delete(c.pending, response.ID)
		pending.result <- benchmarkRequestResult{response: response}
		return true
	}
	if c.registering > 0 {
		if _, duplicate := c.early[response.ID]; duplicate {
			c.failLocked(fmt.Errorf("received duplicate response for request id %d", response.ID))
			return false
		}
		// SendRequest returns the MID after writing, so a fast response can arrive
		// while the sender is still between the write and local registration.
		c.early[response.ID] = response
		return true
	}
	c.failLocked(fmt.Errorf("received response for unknown or completed request id %d", response.ID))
	return false
}

func (c *benchmarkClient) fail(err error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.failLocked(err)
}

func (c *benchmarkClient) failLocked(err error) {
	if c.failure != nil {
		return
	}
	if err == nil {
		err = errors.New("benchmark client failed")
	}
	c.failure = err
	for id, pending := range c.pending {
		delete(c.pending, id)
		pending.result <- benchmarkRequestResult{err: err}
	}
	clear(c.early)
	close(c.failed)
}

func (c *benchmarkClient) failureError() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.failure
}

func (c *benchmarkClient) request(route string, payload []byte) error {
	timer := time.NewTimer(c.requestWait)
	defer timer.Stop()

	select {
	case c.inflight <- struct{}{}:
		defer func() { <-c.inflight }()
	case <-c.failed:
		return c.failureError()
	case <-timer.C:
		err := fmt.Errorf("request to %s timed out waiting for an inflight slot", route)
		c.fail(err)
		return err
	}

	pending := &benchmarkPendingRequest{result: make(chan benchmarkRequestResult, 1)}
	c.stateMu.Lock()
	if c.failure != nil {
		err := c.failure
		c.stateMu.Unlock()
		return err
	}
	c.registering++
	c.stateMu.Unlock()

	// Do not hold stateMu here. SendRequest can block on the client's inflight
	// limiter; benchmarkMaxInflight keeps this call below that blocking edge.
	id, sendErr := c.sendRequest(route, payload)
	if err := c.finishSend(id, route, pending, sendErr); err != nil {
		return err
	}

	for {
		select {
		case result := <-pending.result:
			if result.err != nil {
				return result.err
			}
			if err := c.failureError(); err != nil {
				return err
			}
			response := result.response
			if response.ID != id {
				err := fmt.Errorf("response id = %d, want %d", response.ID, id)
				c.fail(err)
				return err
			}
			if response.Type != pitayamessage.Response || response.Err {
				err := fmt.Errorf("request returned %s", response)
				c.fail(err)
				return err
			}
			return nil
		case <-c.failed:
			return c.failureError()
		case <-timer.C:
			err := fmt.Errorf("request %d to %s timed out", id, route)
			if c.timeout(id, pending, err) {
				return err
			}
			// A response or another terminal failure won while the timer fired.
		}
	}
}

func (c *benchmarkClient) finishSend(id uint, route string, pending *benchmarkPendingRequest, sendErr error) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.registering--

	if c.failure != nil {
		return c.failure
	}
	if sendErr != nil {
		c.failLocked(fmt.Errorf("send request %d to %s: %w", id, route, sendErr))
		return c.failure
	}
	if response, found := c.early[id]; found {
		delete(c.early, id)
		pending.result <- benchmarkRequestResult{response: response}
	} else {
		c.pending[id] = pending
	}

	if c.registering == 0 && len(c.early) > 0 {
		var unknownID uint
		for unknownID = range c.early {
			break
		}
		c.failLocked(fmt.Errorf("received response for unknown request id %d", unknownID))
		return c.failure
	}
	return nil
}

func (c *benchmarkClient) timeout(id uint, pending *benchmarkPendingRequest, err error) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if registered := c.pending[id]; registered != pending {
		return false
	}
	delete(c.pending, id)
	c.failLocked(err)
	return true
}

func (c *benchmarkClient) handleDisconnect(reason client.CloseReason) {
	select {
	case <-c.done:
		return
	default:
		c.fail(fmt.Errorf("benchmark client disconnected with reason %d", reason))
	}
}

func (c *benchmarkClient) close() {
	c.closeOnce.Do(func() {
		c.fail(errors.New("benchmark client closed"))
		close(c.done)
		c.Disconnect(client.CloseReasonManual)
	})
}

func TestMain(m *testing.M) {
	flag.Parse()
	addr = net.JoinHostPort(*host, strconv.Itoa(*port))
	os.Exit(m.Run())
}

func TestBenchmarkTargetReady(t *testing.T) {
	if !*readinessProbe {
		t.Skip("benchmark readiness probe only")
	}
	c := client.New()
	if err := c.ConnectTo(addr); err != nil {
		t.Fatalf("connect to %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Disconnect(client.CloseReasonManual) })

	assertRequestReady(t, c, connectorRawRoute, []byte("ready"))
	assertRequestReady(t, c, gameSessionRoute, []byte{})
}

func assertRequestReady(t *testing.T, c *client.Client, route string, payload []byte) {
	t.Helper()
	id, err := c.SendRequest(route, payload)
	if err != nil {
		t.Fatalf("send readiness request %s: %v", route, err)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case response := <-c.IncomingMsgChan:
		if response == nil {
			t.Fatalf("readiness request %s returned a nil response", route)
		}
		if response.ID != id {
			t.Fatalf("readiness request %s response id = %d, want %d", route, response.ID, id)
		}
		if response.Type != pitayamessage.Response || response.Err {
			t.Fatalf("readiness request %s returned %s", route, response)
		}
	case <-timer.C:
		t.Fatalf("readiness request %s timed out", route)
	}
}

func getClients(b *testing.B, n int) []*benchmarkClient {
	b.Helper()
	clients := make([]*benchmarkClient, n)
	b.Cleanup(func() {
		for _, c := range clients {
			if c != nil {
				c.close()
			}
		}
	})
	for i := range clients {
		clients[i] = newBenchmarkClient()
		if err := clients[i].connect(addr); err != nil {
			b.Fatalf("connect benchmark client %d to %s: %v", i, addr, err)
		}
	}
	return clients
}

func sendBenchmarkRequest(c *benchmarkClient, route string, payload []byte) error {
	return c.request(route, payload)
}

func runSequentialRequests(b *testing.B, c *benchmarkClient, route string, payload []byte) {
	b.Helper()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sendBenchmarkRequest(c, route, payload); err != nil {
			b.Fatalf("request to %s failed at iteration %d: %v", route, i, err)
		}
	}
	if err := c.failureError(); err != nil {
		b.Fatalf("request to %s left the benchmark client failed: %v", route, err)
	}
}

func runParallelRequests(b *testing.B, clients []*benchmarkClient, route string, payload []byte, distribute bool) {
	b.Helper()
	var nextClient atomic.Uint64
	var firstErr error
	var errOnce sync.Once
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c := clients[0]
			if distribute {
				index := nextClient.Add(1) - 1
				c = clients[index%uint64(len(clients))]
			}
			if err := sendBenchmarkRequest(c, route, payload); err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
		}
	})
	if firstErr != nil {
		b.Fatalf("parallel request to %s failed: %v", route, firstErr)
	}
	for i, c := range clients {
		if err := c.failureError(); err != nil {
			b.Fatalf("parallel request to %s left client %d failed: %v", route, i, err)
		}
	}
}

func BenchmarkCreateManyClients(b *testing.B) {
	clients := make([]*benchmarkClient, b.N)
	b.Cleanup(func() {
		for _, c := range clients {
			if c != nil {
				c.close()
			}
		}
	})
	b.ResetTimer()
	for i := range clients {
		clients[i] = newBenchmarkClient()
		if err := clients[i].connect(addr); err != nil {
			b.Fatalf("connect benchmark client %d to %s: %v", i, addr, err)
		}
	}
}

func BenchmarkFrontHandlerWithSessionAndRawReturnsRaw(b *testing.B) {
	runSequentialRequests(b, getClients(b, 1)[0], connectorRawRoute, []byte("ola"))
}

func BenchmarkFrontHandlerWithSessionAndPtrReturnsPtr(b *testing.B) {
	runSequentialRequests(b, getClients(b, 1)[0], connectorPtrRoute, []byte(`{"msg":"bench single"}`))
}

func BenchmarkFrontHandlerWithSessionAndPtrReturnsPtrManyClientsParallel(b *testing.B) {
	runParallelRequests(b, getClients(b, 1000), connectorPtrRoute, []byte(`{"msg":"bench parallel"}`), true)
}

func BenchmarkFrontHandlerWithSessionAndPtrReturnsPtrParallel(b *testing.B) {
	runParallelRequests(b, getClients(b, 1), connectorPtrRoute, []byte(`{"msg":"b"}`), false)
}

func BenchmarkFrontHandlerWithSessionOnlyReturnsPtr(b *testing.B) {
	runSequentialRequests(b, getClients(b, 1)[0], connectorSessionRoute, []byte{})
}

func BenchmarkFrontHandlerWithSessionOnlyReturnsPtrParallel(b *testing.B) {
	runParallelRequests(b, getClients(b, 1), connectorSessionRoute, []byte{}, false)
}

func BenchmarkBackHandlerWithSessionOnlyReturnsPtr(b *testing.B) {
	runSequentialRequests(b, getClients(b, 1)[0], gameSessionRoute, []byte{})
}

func BenchmarkBackHandlerWithSessionOnlyReturnsPtrParallel(b *testing.B) {
	runParallelRequests(b, getClients(b, 1), gameSessionRoute, []byte{}, false)
}

func BenchmarkBackHandlerWithSessionOnlyReturnsPtrParallelMultipleClients(b *testing.B) {
	runParallelRequests(b, getClients(b, 100), gameSessionRoute, []byte{}, true)
}
