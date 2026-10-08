package client

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type generationTestAddr string

func (a generationTestAddr) Network() string { return "test" }
func (a generationTestAddr) String() string  { return string(a) }

type generationTestConn struct {
	readRelease <-chan struct{}
	write       func([]byte) (int, error)
	closed      chan struct{}
	closeOnce   sync.Once
}

func newGenerationTestConn() *generationTestConn {
	return &generationTestConn{closed: make(chan struct{})}
}

func (c *generationTestConn) Read([]byte) (int, error) {
	if c.readRelease != nil {
		<-c.readRelease
	}
	return 0, io.EOF
}

func (c *generationTestConn) Write(data []byte) (int, error) {
	if c.write != nil {
		return c.write(data)
	}
	return len(data), nil
}

func (c *generationTestConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (*generationTestConn) LocalAddr() net.Addr              { return generationTestAddr("local") }
func (*generationTestConn) RemoteAddr() net.Addr             { return generationTestAddr("remote") }
func (*generationTestConn) SetDeadline(time.Time) error      { return nil }
func (*generationTestConn) SetReadDeadline(time.Time) error  { return nil }
func (*generationTestConn) SetWriteDeadline(time.Time) error { return nil }

type generationMessageEncoder struct {
	err error
}

func (*generationMessageEncoder) IsCompressionEnabled() bool { return false }
func (e *generationMessageEncoder) Encode(*message.Message) ([]byte, error) {
	return nil, e.err
}

type generationPacketEncoder struct {
	err error
}

func (e *generationPacketEncoder) Encode(packet.Type, []byte) ([]byte, error) {
	return nil, e.err
}

func installTestGeneration(c *Client, conn net.Conn, startLoops bool, heartbeat int) *connectionGeneration {
	c.connMutex.Lock()
	gen := newConnectionGeneration(conn, c.packetEncoder, c.messageEncoder)
	c.current = gen
	c.IncomingMsgChan = gen.incomingChan
	c.state = clientStateConnected
	c.Connected = true
	if startLoops {
		c.startGenerationLoops(gen, heartbeat)
	}
	c.connMutex.Unlock()
	return gen
}

func pendingState(gen *connectionGeneration) (int, int) {
	gen.pendingMu.Lock()
	defer gen.pendingMu.Unlock()
	return len(gen.pending), len(gen.slots)
}

func waitForGenerationLoops(t *testing.T, gen *connectionGeneration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		gen.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("generation loops did not finish")
	}
}

func TestStaleGenerationCannotDisconnectReplacement(t *testing.T) {
	readRelease := make(chan struct{})
	oldConn := newGenerationTestConn()
	oldConn.readRelease = readRelease
	c := New()
	oldGen := installTestGeneration(c, oldConn, true, 0)

	newConn := newGenerationTestConn()
	var callbacks atomic.Int64
	var newGen *connectionGeneration
	c.OnDisconnected(func(CloseReason) {
		if callbacks.Add(1) == 1 {
			newGen = installTestGeneration(c, newConn, false, 0)
		}
	})

	c.disconnectGeneration(oldGen, CloseReasonManual)
	require.NotNil(t, newGen)
	close(readRelease)
	waitForGenerationLoops(t, oldGen)

	assert.True(t, c.ConnectedStatus())
	assert.Same(t, newGen, c.currentGeneration())
	assert.Equal(t, int64(1), callbacks.Load())
	select {
	case <-newGen.closeCh:
		t.Fatal("stale generation closed the replacement")
	default:
	}

	c.OnDisconnected(nil)
	c.Disconnect(CloseReasonManual)
}

func TestManualDisconnectAndReadErrorInvokeCallbackOnce(t *testing.T) {
	readRelease := make(chan struct{})
	conn := newGenerationTestConn()
	conn.readRelease = readRelease
	c := New()
	gen := installTestGeneration(c, conn, true, 0)

	var callbacks atomic.Int64
	c.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	disconnectDone := make(chan struct{})
	go func() {
		c.Disconnect(CloseReasonManual)
		close(disconnectDone)
	}()
	close(readRelease)

	select {
	case <-disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not return")
	}
	waitForGenerationLoops(t, gen)
	assert.Equal(t, int64(1), callbacks.Load())
}

func TestDisconnectUnblocksTimeoutDeliveryAndRestoresInflightSlots(t *testing.T) {
	c := New(time.Millisecond)
	gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
	for range cap(gen.incomingChan) {
		gen.incomingChan <- &message.Message{Type: message.Push}
	}
	for id := uint(1); id <= 11; id++ {
		require.NoError(t, gen.registerPending(&pendingRequest{
			msg:    &message.Message{Type: message.Request, ID: id, Route: "test.route"},
			sentAt: time.Now().Add(-time.Second),
		}))
	}

	expired := gen.takeExpiredPending(time.Now(), c.requestTimeout)
	require.Len(t, expired, 11)
	deliveryDone := make(chan struct{})
	go func() {
		defer close(deliveryDone)
		for _, pending := range expired {
			if !gen.deliver(&message.Message{Type: message.Response, ID: pending.msg.ID, Err: true}) {
				return
			}
		}
	}()

	c.Disconnect(CloseReasonManual)
	select {
	case <-deliveryDone:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not unblock timeout delivery")
	}
	pending, slots := pendingState(gen)
	assert.Zero(t, pending)
	assert.Zero(t, slots)
}

func TestSendFailuresDoNotLeavePendingRequests(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*connectionGeneration, *generationTestConn)
	}{
		{
			name: "message encoder",
			configure: func(gen *connectionGeneration, _ *generationTestConn) {
				gen.messageEncoder = &generationMessageEncoder{err: errors.New("message encode failed")}
			},
		},
		{
			name: "packet encoder",
			configure: func(gen *connectionGeneration, _ *generationTestConn) {
				gen.packetEncoder = &generationPacketEncoder{err: errors.New("packet encode failed")}
			},
		},
		{
			name: "write",
			configure: func(_ *connectionGeneration, conn *generationTestConn) {
				conn.write = func([]byte) (int, error) { return 0, errors.New("write failed") }
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := New()
			conn := newGenerationTestConn()
			gen := installTestGeneration(c, conn, false, 0)
			test.configure(gen, conn)

			_, err := c.SendRequest("test.route", nil)
			require.Error(t, err)
			pending, slots := pendingState(gen)
			assert.Zero(t, pending)
			assert.Zero(t, slots)
			select {
			case msg := <-gen.incomingChan:
				t.Fatalf("unexpected completion after send failure: %#v", msg)
			default:
			}
			c.Disconnect(CloseReasonManual)
		})
	}
}

func TestRequestIsPendingBeforeWriteCanDeliverImmediateResponse(t *testing.T) {
	c := New()
	conn := newGenerationTestConn()
	gen := installTestGeneration(c, conn, false, 0)
	responseDelivered := make(chan struct{})
	conn.write = func(data []byte) (int, error) {
		pending, ok := gen.takePending(1)
		if !ok {
			return 0, errors.New("response arrived before request registration")
		}
		gen.incomingChan <- &message.Message{
			Type:  message.Response,
			ID:    pending.msg.ID,
			Route: pending.msg.Route,
		}
		close(responseDelivered)
		return len(data), nil
	}

	id, err := c.SendRequest("test.route", nil)
	require.NoError(t, err)
	assert.Equal(t, uint(1), id)
	select {
	case <-responseDelivered:
	case <-time.After(time.Second):
		t.Fatal("immediate response was not delivered")
	}
	response := <-gen.incomingChan
	assert.Equal(t, id, response.ID)
	pending, slots := pendingState(gen)
	assert.Zero(t, pending)
	assert.Zero(t, slots)
	c.Disconnect(CloseReasonManual)
}

func TestDisconnectRejectsInflightSlotWaiter(t *testing.T) {
	c := New()
	gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
	for id := uint(1); id <= clientInflightLimit; id++ {
		require.NoError(t, gen.registerPending(&pendingRequest{
			msg:    &message.Message{Type: message.Request, ID: id},
			sentAt: time.Now(),
		}))
	}

	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- gen.registerPending(&pendingRequest{
			msg:    &message.Message{Type: message.Request, ID: clientInflightLimit + 1},
			sentAt: time.Now(),
		})
	}()
	<-started
	c.Disconnect(CloseReasonManual)

	select {
	case err := <-result:
		require.ErrorIs(t, err, errClientConnectionClosed)
	case <-time.After(time.Second):
		t.Fatal("slot waiter did not stop after disconnect")
	}
	pending, slots := pendingState(gen)
	assert.Zero(t, pending)
	assert.Zero(t, slots)
}

func TestConcurrentConnectAdmissionAndDisconnectDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	handshakeRead := make(chan struct{})
	releaseHandshake := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		if _, readErr := readPacket(conn); readErr != nil {
			return
		}
		close(handshakeRead)
		<-releaseHandshake
	}()

	c := New()
	connectResult := make(chan error, 1)
	go func() { connectResult <- c.ConnectTo(listener.Addr().String()) }()
	select {
	case <-handshakeRead:
	case <-time.After(time.Second):
		t.Fatal("server did not receive handshake")
	}

	require.ErrorIs(t, c.ConnectTo(listener.Addr().String()), errClientConnecting)
	var callbacks atomic.Int64
	c.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	disconnectDone := make(chan struct{})
	go func() {
		c.Disconnect(CloseReasonManual)
		close(disconnectDone)
	}()

	select {
	case err := <-connectResult:
		require.ErrorIs(t, err, errClientConnectionClosed)
	case <-time.After(time.Second):
		t.Fatal("cancelled connect attempt did not return")
	}
	select {
	case <-disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not join the connect attempt")
	}
	assert.False(t, c.ConnectedStatus())
	assert.Zero(t, callbacks.Load())
	close(releaseHandshake)
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("handshake server did not stop")
	}
}

func TestConnectContextCancellationIsPreserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New()
	_, err := c.connectToContext(ctx, "tcp://127.0.0.1:1", nil)
	require.ErrorIs(t, err, context.Canceled)
}
