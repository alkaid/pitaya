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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/client"
	pitayamessage "github.com/alkaid/pitaya/v2/conn/message"
)

func benchmarkResponse(id uint) *pitayamessage.Message {
	return &pitayamessage.Message{ID: id, Type: pitayamessage.Response}
}

func TestBenchmarkClientResponseBeforeRegistration(t *testing.T) {
	c := newBenchmarkClient()
	c.requestWait = time.Second
	entered := make(chan struct{})
	release := make(chan struct{})
	c.sendRequest = func(string, []byte) (uint, error) {
		close(entered)
		<-release
		return 7, nil
	}

	requestDone := make(chan error, 1)
	go func() { requestDone <- c.request("test.route", nil) }()
	<-entered

	handled := make(chan bool, 1)
	go func() { handled <- c.handleResponse(benchmarkResponse(7)) }()
	select {
	case ok := <-handled:
		if !ok {
			t.Fatal("early response unexpectedly failed the benchmark client")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("response dispatch blocked while SendRequest was in progress")
	}

	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("request with an early response failed: %v", err)
	}
}

func TestBenchmarkClientRejectsDuplicateAndUnknownResponses(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		c := newBenchmarkClient()
		entered := make(chan struct{})
		release := make(chan struct{})
		c.sendRequest = func(string, []byte) (uint, error) {
			close(entered)
			<-release
			return 11, nil
		}

		requestDone := make(chan error, 1)
		go func() { requestDone <- c.request("test.route", nil) }()
		<-entered
		if !c.handleResponse(benchmarkResponse(11)) {
			t.Fatal("first early response was rejected")
		}
		if c.handleResponse(benchmarkResponse(11)) {
			t.Fatal("duplicate early response was accepted")
		}
		close(release)

		err := <-requestDone
		if err == nil || !strings.Contains(err.Error(), "duplicate response") {
			t.Fatalf("request error = %v, want duplicate response failure", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		c := newBenchmarkClient()
		if c.handleResponse(benchmarkResponse(99)) {
			t.Fatal("unknown response was accepted")
		}
		err := c.failureError()
		if err == nil || !strings.Contains(err.Error(), "unknown or completed") {
			t.Fatalf("failure = %v, want unknown response failure", err)
		}
	})
}

func TestBenchmarkClientUnknownEarlyResponseFailsAfterRegistration(t *testing.T) {
	c := newBenchmarkClient()
	entered := make(chan struct{})
	release := make(chan struct{})
	c.sendRequest = func(string, []byte) (uint, error) {
		close(entered)
		<-release
		return 13, nil
	}

	requestDone := make(chan error, 1)
	go func() { requestDone <- c.request("test.route", nil) }()
	<-entered
	if !c.handleResponse(benchmarkResponse(404)) {
		t.Fatal("response could not be deferred during registration")
	}
	close(release)

	err := <-requestDone
	if err == nil || !strings.Contains(err.Error(), "unknown request id 404") {
		t.Fatalf("request error = %v, want unknown early response failure", err)
	}
}

func TestBenchmarkClientTimeoutIsTerminal(t *testing.T) {
	c := newBenchmarkClient()
	c.requestWait = 25 * time.Millisecond
	c.sendRequest = func(string, []byte) (uint, error) { return 17, nil }

	err := c.request("test.route", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("request error = %v, want timeout", err)
	}
	if got := c.failureError(); got == nil || (!errors.Is(got, err) && got.Error() != err.Error()) {
		t.Fatalf("client failure = %v, want %v", got, err)
	}

	c.stateMu.Lock()
	pending := len(c.pending)
	c.stateMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending requests after timeout = %d, want 0", pending)
	}
}

func TestBenchmarkClientSendFailureIsTerminal(t *testing.T) {
	c := newBenchmarkClient()
	sendFailure := errors.New("synthetic write failure")
	c.sendRequest = func(string, []byte) (uint, error) { return 23, sendFailure }

	err := c.request("test.route", nil)
	if err == nil || !errors.Is(err, sendFailure) {
		t.Fatalf("request error = %v, want wrapped send failure", err)
	}
	c.stateMu.Lock()
	registering := c.registering
	pending := len(c.pending)
	c.stateMu.Unlock()
	if registering != 0 || pending != 0 {
		t.Fatalf("state after send failure: registering=%d pending=%d, want both zero", registering, pending)
	}
}

func TestBenchmarkClientDisconnectWakesPending(t *testing.T) {
	c := newBenchmarkClient()
	c.requestWait = time.Second
	c.sendRequest = func(string, []byte) (uint, error) { return 19, nil }

	requestDone := make(chan error, 1)
	go func() { requestDone <- c.request("test.route", nil) }()
	waitForBenchmarkPending(t, c, 19)
	c.handleDisconnect(client.CloseReasonError)

	select {
	case err := <-requestDone:
		if err == nil || !strings.Contains(err.Error(), "disconnected") {
			t.Fatalf("request error = %v, want disconnect failure", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("pending request was not woken by disconnect")
	}
}

func TestBenchmarkClientCloseWakesPending(t *testing.T) {
	c := newBenchmarkClient()
	c.requestWait = time.Second
	c.sendRequest = func(string, []byte) (uint, error) { return 20, nil }

	requestDone := make(chan error, 1)
	go func() { requestDone <- c.request("test.route", nil) }()
	waitForBenchmarkPending(t, c, 20)
	c.close()

	select {
	case err := <-requestDone:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("request error = %v, want close failure", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("pending request was not woken by close")
	}
}

func TestBenchmarkClientInflightGateWakesOnFailure(t *testing.T) {
	c := newBenchmarkClient()
	c.requestWait = time.Second
	entered := make(chan struct{}, benchmarkMaxInflight+1)
	release := make(chan struct{})
	var nextID atomic.Uint32
	c.sendRequest = func(string, []byte) (uint, error) {
		entered <- struct{}{}
		<-release
		return uint(nextID.Add(1)), nil
	}

	results := make(chan error, benchmarkMaxInflight+1)
	for i := 0; i < benchmarkMaxInflight+1; i++ {
		go func() { results <- c.request("test.route", nil) }()
	}
	for i := 0; i < benchmarkMaxInflight; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatalf("only %d sends entered the client", i)
		}
	}
	select {
	case <-entered:
		t.Fatal("benchmark admitted a request beyond the client inflight cap")
	case <-time.After(50 * time.Millisecond):
	}

	c.fail(errors.New("synthetic connection failure"))
	select {
	case err := <-results:
		if err == nil || !strings.Contains(err.Error(), "synthetic connection failure") {
			t.Fatalf("waiting request error = %v, want connection failure", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("request waiting at the inflight gate was not woken by failure")
	}

	close(release)
	for i := 0; i < benchmarkMaxInflight; i++ {
		if err := <-results; err == nil {
			t.Fatal("inflight request unexpectedly succeeded after client failure")
		}
	}
}

func TestBenchmarkClientResponseTimeoutRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := newBenchmarkClient()
		id := uint(i + 1)
		pending := &benchmarkPendingRequest{result: make(chan benchmarkRequestResult, 1)}
		c.pending[id] = pending
		start := make(chan struct{})
		responseWon := make(chan bool, 1)
		timeoutWon := make(chan bool, 1)
		go func() {
			<-start
			responseWon <- c.handleResponse(benchmarkResponse(id))
		}()
		go func() {
			<-start
			timeoutWon <- c.timeout(id, pending, fmt.Errorf("request %d timed out", id))
		}()
		close(start)

		responseHandled := <-responseWon
		timedOut := <-timeoutWon
		if responseHandled == timedOut {
			t.Fatalf("iteration %d: response handled=%t, timeout won=%t; want exactly one winner", i, responseHandled, timedOut)
		}
	}
}

func waitForBenchmarkPending(t *testing.T, c *benchmarkClient, id uint) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.stateMu.Lock()
		_, found := c.pending[id]
		c.stateMu.Unlock()
		if found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("request %d was not registered as pending", id)
}
