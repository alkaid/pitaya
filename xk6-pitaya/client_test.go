package pitaya

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pitayaclient "github.com/alkaid/pitaya/v2/client"
	pitayamessage "github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/grafana/sobek"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/lib"
)

type pendingTestVU struct {
	runtime   *sobek.Runtime
	ctx       context.Context
	callbacks chan func() error
}

func newPendingTestVU(ctx context.Context) *pendingTestVU {
	return &pendingTestVU{
		runtime:   sobek.New(),
		ctx:       ctx,
		callbacks: make(chan func() error, 4096),
	}
}

func (v *pendingTestVU) Context() context.Context         { return v.ctx }
func (v *pendingTestVU) Events() common.Events            { return common.Events{} }
func (v *pendingTestVU) InitEnv() *common.InitEnvironment { return nil }
func (v *pendingTestVU) State() *lib.State                { return nil }
func (v *pendingTestVU) Runtime() *sobek.Runtime          { return v.runtime }
func (v *pendingTestVU) RegisterCallback() func(func() error) {
	return func(callback func() error) { v.callbacks <- callback }
}

type pendingTestPitayaClient struct {
	mu             sync.Mutex
	messages       chan *pitayamessage.Message
	sendRequest    func(string, []byte) (uint, error)
	onDisconnected func(pitayaclient.CloseReason)
	connected      bool
	disconnects    atomic.Int64
}

func (c *pendingTestPitayaClient) ConnectTo(string, ...*tls.Config) error {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	return nil
}

func (c *pendingTestPitayaClient) ConnectedStatus() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *pendingTestPitayaClient) Disconnect(reason pitayaclient.CloseReason) {
	c.disconnects.Add(1)
	c.mu.Lock()
	c.connected = false
	callback := c.onDisconnected
	c.mu.Unlock()
	if callback != nil {
		callback(reason)
	}
}

func (c *pendingTestPitayaClient) MsgChannel() chan *pitayamessage.Message {
	return c.messages
}

func (c *pendingTestPitayaClient) SendNotify(string, []byte) error { return nil }

func (c *pendingTestPitayaClient) SendRequest(route string, data []byte) (uint, error) {
	return c.sendRequest(route, data)
}

func (c *pendingTestPitayaClient) SetClientHandshakeData(*session.HandshakeData) {}

func (c *pendingTestPitayaClient) OnDisconnected(callback func(pitayaclient.CloseReason)) {
	c.mu.Lock()
	c.onDisconnected = callback
	c.mu.Unlock()
}

func newPendingTestClient(t *testing.T, timeout time.Duration, send func(string, []byte) (uint, error)) (*Client, *pendingTestPitayaClient, *pendingTestVU) {
	t.Helper()
	vu := newPendingTestVU(context.Background())
	transport := &pendingTestPitayaClient{
		messages:    make(chan *pitayamessage.Message),
		sendRequest: send,
		connected:   true,
	}
	client := &Client{
		vu:                vu,
		client:            transport,
		responses:         make(map[uint]*pendingResponse),
		acceptingRequests: true,
		pushes:            make(map[string]chan []byte),
		timeout:           timeout,
	}
	transport.OnDisconnected(func(reason pitayaclient.CloseReason) {
		client.handleDisconnected(0, reason)
	})
	stop := make(chan struct{})
	client.listenerStop = stop
	go client.listen(transport.messages, stop)
	t.Cleanup(func() {
		client.registrationMu.Lock()
		client.stopListenerLocked()
		client.registrationMu.Unlock()
	})
	return client, transport, vu
}

func awaitPromise(t *testing.T, vu *pendingTestVU, promise *sobek.Promise, want sobek.PromiseState) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for promise.State() == sobek.PromiseStatePending {
		select {
		case callback := <-vu.callbacks:
			if err := callback(); err != nil {
				t.Fatalf("promise callback failed: %v", err)
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for promise completion")
		}
	}
	if got := promise.State(); got != want {
		t.Fatalf("promise state = %v, want %v", got, want)
	}
}

func pendingResponseCount(client *Client) int {
	client.registrationMu.Lock()
	defer client.registrationMu.Unlock()
	return len(client.responses)
}

func waitForPendingCount(t *testing.T, client *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if pendingResponseCount(client) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending responses = %d, want %d", pendingResponseCount(client), want)
}

func TestRequestRegistersBeforeImmediateResponseDelivery(t *testing.T) {
	var transport *pendingTestPitayaClient
	client, createdTransport, vu := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
		transport.messages <- &pitayamessage.Message{
			Type: pitayamessage.Response,
			ID:   41,
			Data: []byte(`{"ok":true}`),
		}
		return 41, nil
	})
	transport = createdTransport

	promise := client.Request("room.entry", map[string]interface{}{"room": 1})
	awaitPromise(t, vu, promise, sobek.PromiseStateFulfilled)
	waitForPendingCount(t, client, 0)

	result, ok := promise.Result().Export().(map[string]interface{})
	if !ok || result["ok"] != true {
		t.Fatalf("promise result = %#v, want decoded response", promise.Result().Export())
	}
}

func TestRequestCleansPendingOnEveryTerminalPath(t *testing.T) {
	t.Run("send error", func(t *testing.T) {
		sendErr := errors.New("send failed")
		client, _, vu := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
			return 1, sendErr
		})
		promise := client.Request("room.entry", nil)
		awaitPromise(t, vu, promise, sobek.PromiseStateRejected)
		waitForPendingCount(t, client, 0)
	})

	t.Run("invalid json response", func(t *testing.T) {
		client, _, vu := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
			return 2, nil
		})
		promise := client.Request("room.entry", nil)
		client.deliverResponse(2, []byte("not-json"))
		awaitPromise(t, vu, promise, sobek.PromiseStateFulfilled)
		waitForPendingCount(t, client, 0)
	})

	t.Run("timeout and late response", func(t *testing.T) {
		client, _, vu := newPendingTestClient(t, 5*time.Millisecond, func(string, []byte) (uint, error) {
			return 3, nil
		})
		promise := client.Request("room.entry", nil)
		awaitPromise(t, vu, promise, sobek.PromiseStateRejected)
		waitForPendingCount(t, client, 0)
		client.deliverResponse(3, []byte(`{"late":true}`))
		if got := pendingResponseCount(client); got != 0 {
			t.Fatalf("late response recreated pending entry: %d", got)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		vu := newPendingTestVU(ctx)
		transport := &pendingTestPitayaClient{
			messages: make(chan *pitayamessage.Message),
			sendRequest: func(string, []byte) (uint, error) {
				return 4, nil
			},
			connected: true,
		}
		client := &Client{
			vu:                vu,
			client:            transport,
			responses:         make(map[uint]*pendingResponse),
			acceptingRequests: true,
			timeout:           time.Second,
		}
		promise := client.Request("room.entry", nil)
		cancel()
		awaitPromise(t, vu, promise, sobek.PromiseStateRejected)
		waitForPendingCount(t, client, 0)
	})
}

func TestRequestDisconnectRejectsAllPending(t *testing.T) {
	var next atomic.Uint64
	client, transport, vu := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
		return uint(next.Add(1)), nil
	})
	promises := []*sobek.Promise{
		client.Request("room.one", nil),
		client.Request("room.two", nil),
	}
	waitForPendingCount(t, client, len(promises))
	client.Disconnect()
	for _, promise := range promises {
		awaitPromise(t, vu, promise, sobek.PromiseStateRejected)
	}
	waitForPendingCount(t, client, 0)
	if got := transport.disconnects.Load(); got != 1 {
		t.Fatalf("disconnect calls = %d, want 1", got)
	}
}

func TestStaleDisconnectCallbackDoesNotFailNewConnectionGeneration(t *testing.T) {
	client, _, _ := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
		return 1, nil
	})
	slot := &pendingResponse{result: make(chan pendingResponseResult, 1)}
	client.registrationMu.Lock()
	client.connectionGeneration = 2
	client.responses[1] = slot
	client.registrationMu.Unlock()

	client.handleDisconnected(1, pitayaclient.CloseReasonError)
	if got := pendingResponseCount(client); got != 1 {
		t.Fatalf("pending responses after stale callback = %d, want 1", got)
	}
	client.registrationMu.Lock()
	accepting := client.acceptingRequests
	client.registrationMu.Unlock()
	if !accepting {
		t.Fatal("stale callback stopped the current connection generation")
	}

	client.removePendingResponse(1, slot)
}

func TestDuplicateResponseDoesNotBlockListener(t *testing.T) {
	client, _, vu := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
		return 12, nil
	})
	promise := client.Request("room.entry", nil)
	client.deliverResponse(12, []byte(`{"result":"first"}`))
	client.deliverResponse(12, []byte(`{"result":"duplicate"}`))
	awaitPromise(t, vu, promise, sobek.PromiseStateFulfilled)
	waitForPendingCount(t, client, 0)
}

func TestRequestIDCollisionFailsConnectionAndAllPromises(t *testing.T) {
	client, transport, vu := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
		return 7, nil
	})
	first := client.Request("room.first", nil)
	waitForPendingCount(t, client, 1)
	second := client.Request("room.second", nil)

	awaitPromise(t, vu, first, sobek.PromiseStateRejected)
	awaitPromise(t, vu, second, sobek.PromiseStateRejected)
	waitForPendingCount(t, client, 0)
	if got := transport.disconnects.Load(); got != 1 {
		t.Fatalf("disconnect calls = %d, want 1", got)
	}
	client.registrationMu.Lock()
	accepting := client.acceptingRequests
	err := client.connectionErr
	client.registrationMu.Unlock()
	if accepting || !errors.Is(err, errRequestIDCollision) {
		t.Fatalf("connection state = accepting %t, error %v", accepting, err)
	}
}

func TestConcurrentPendingResponsesDoNotCrossDeliver(t *testing.T) {
	var next atomic.Uint64
	client, _, _ := newPendingTestClient(t, time.Second, func(string, []byte) (uint, error) {
		return uint(next.Add(1)), nil
	})

	const requests = 128
	type registered struct {
		id   uint
		slot *pendingResponse
	}
	registeredRequests := make(chan registered, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot := &pendingResponse{result: make(chan pendingResponseResult, 1)}
			id, err, protocolFailure := client.sendAndRegister("room.concurrent", nil, slot)
			if err != nil || protocolFailure {
				t.Errorf("sendAndRegister() = (%d, %v, %t)", id, err, protocolFailure)
				return
			}
			registeredRequests <- registered{id: id, slot: slot}
		}()
	}
	wg.Wait()
	close(registeredRequests)

	for request := range registeredRequests {
		data := []byte{byte(request.id)}
		client.deliverResponse(request.id, data)
		result := <-request.slot.result
		if result.err != nil || len(result.data) != 1 || result.data[0] != byte(request.id) {
			t.Fatalf("response for ID %d = (%v, %v)", request.id, result.data, result.err)
		}
		client.removePendingResponse(request.id, request.slot)
	}
	waitForPendingCount(t, client, 0)
}

func TestManyRequestTimeoutsReturnPendingMapToBaseline(t *testing.T) {
	var next atomic.Uint64
	client, _, vu := newPendingTestClient(t, time.Millisecond, func(string, []byte) (uint, error) {
		return uint(next.Add(1)), nil
	})
	const requests = 128
	promises := make([]*sobek.Promise, 0, requests)
	for range requests {
		promises = append(promises, client.Request("room.timeout", nil))
	}
	for _, promise := range promises {
		awaitPromise(t, vu, promise, sobek.PromiseStateRejected)
	}
	waitForPendingCount(t, client, 0)
}

func BenchmarkPendingResponseRegistration(b *testing.B) {
	var next atomic.Uint64
	transport := &pendingTestPitayaClient{
		sendRequest: func(string, []byte) (uint, error) {
			return uint(next.Add(1)), nil
		},
		connected: true,
	}
	client := &Client{
		client:            transport,
		responses:         make(map[uint]*pendingResponse),
		acceptingRequests: true,
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slot := &pendingResponse{result: make(chan pendingResponseResult, 1)}
			id, err, protocolFailure := client.sendAndRegister("room.benchmark", nil, slot)
			if err != nil || protocolFailure {
				b.Fatalf("sendAndRegister() = (%d, %v, %t)", id, err, protocolFailure)
			}
			client.removePendingResponse(id, slot)
		}
	})
}
