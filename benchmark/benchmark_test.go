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
)

var (
	host           = flag.String("host", "localhost", "server host")
	port           = flag.Int("port", 32222, "server port")
	readinessProbe = flag.Bool("readiness-probe", false, "run the server readiness probe")
	addr           string
)

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

func getClients(b *testing.B, n int) []*client.Client {
	b.Helper()
	clients := make([]*client.Client, n)
	b.Cleanup(func() {
		for _, c := range clients {
			if c != nil {
				c.Disconnect(client.CloseReasonManual)
			}
		}
	})
	for i := range clients {
		clients[i] = client.New()
		if err := clients[i].ConnectTo(addr); err != nil {
			b.Fatalf("connect benchmark client %d to %s: %v", i, addr, err)
		}
	}
	return clients
}

func sendBenchmarkRequest(c *client.Client, route string, payload []byte) error {
	if _, err := c.SendRequest(route, payload); err != nil {
		return err
	}
	response, ok := <-c.IncomingMsgChan
	if !ok {
		return errors.New("incoming message channel closed")
	}
	if response == nil {
		return errors.New("received nil response")
	}
	if response.Type != pitayamessage.Response || response.Err {
		return fmt.Errorf("request returned %s", response)
	}
	return nil
}

func runSequentialRequests(b *testing.B, c *client.Client, route string, payload []byte) {
	b.Helper()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sendBenchmarkRequest(c, route, payload); err != nil {
			b.Fatalf("request to %s failed at iteration %d: %v", route, i, err)
		}
	}
}

func runParallelRequests(b *testing.B, clients []*client.Client, route string, payload []byte, distribute bool) {
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
}

func BenchmarkCreateManyClients(b *testing.B) {
	clients := make([]*client.Client, b.N)
	b.Cleanup(func() {
		for _, c := range clients {
			if c != nil {
				c.Disconnect(client.CloseReasonManual)
			}
		}
	})
	b.ResetTimer()
	for i := range clients {
		clients[i] = client.New()
		if err := clients[i].ConnectTo(addr); err != nil {
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
