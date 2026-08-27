package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type branchDeadlineConn struct {
	net.Conn
	readDeadline  func(time.Time) error
	writeDeadline func(time.Time) error
}

func (c *branchDeadlineConn) SetReadDeadline(deadline time.Time) error {
	if c.readDeadline != nil {
		return c.readDeadline(deadline)
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *branchDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if c.writeDeadline != nil {
		return c.writeDeadline(deadline)
	}
	return c.Conn.SetWriteDeadline(deadline)
}

type branchTimeoutError struct{}

func (branchTimeoutError) Error() string   { return "timeout" }
func (branchTimeoutError) Timeout() bool   { return true }
func (branchTimeoutError) Temporary() bool { return false }

type branchDeadlineContext struct {
	deadline time.Time
}

func (c branchDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (branchDeadlineContext) Done() <-chan struct{}         { return nil }
func (branchDeadlineContext) Err() error                    { return nil }
func (branchDeadlineContext) Value(any) any                 { return nil }

func serveHandshakeResponseOnly(conn net.Conn, heartbeat int) error {
	payload, err := json.Marshal(HandshakeData{Code: 200, Sys: HandshakeSys{Heartbeat: heartbeat}})
	if err != nil {
		return err
	}
	frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Handshake, payload)
	if err != nil {
		return err
	}
	if _, err = conn.Write(frame); err != nil {
		return err
	}
	ack, err := readPacket(conn)
	if err != nil {
		return err
	}
	if ack.Type != packet.HandshakeAck {
		return errors.New("missing handshake ack")
	}
	return nil
}

func startBranchHandshakeServer(t *testing.T) (net.Listener, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		done <- serveClientHandshake(conn)
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, done
}

func TestClientHandshakeAndWriteGuardBranches(t *testing.T) {
	c := New()

	require.ErrorIs(t, c.sendHandshakeRequestTo(nil, nil, c.packetEncoder), errClientConnectionClosed)
	_, err := c.receiveHandshakeResponseFrom(nil, c.packetEncoder)
	require.ErrorIs(t, err, errClientConnectionClosed)
	_, err = writeConnWithTimeout(nil, nil, time.Second)
	require.ErrorIs(t, err, errClientConnectionClosed)

	writeDeadlineErr := errors.New("set write deadline")
	deadlineConn := &branchDeadlineConn{
		Conn: newGenerationTestConn(),
		writeDeadline: func(time.Time) error {
			return writeDeadlineErr
		},
	}
	_, err = writeConnWithTimeout(deadlineConn, []byte("payload"), time.Second)
	require.ErrorIs(t, err, writeDeadlineErr)

	c.conn = nil
	require.ErrorIs(t, c.handleHandshakeResponse(), errClientConnectionClosed)

	for _, test := range []struct {
		name     string
		clearErr error
	}{
		{name: "success"},
		{name: "clear deadline error", clearErr: errors.New("clear read deadline")},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			serverDone := make(chan error, 1)
			go func() { serverDone <- serveHandshakeResponseOnly(serverConn, 7) }()

			wrapped := &branchDeadlineConn{Conn: clientConn}
			if test.clearErr != nil {
				wrapped.readDeadline = func(deadline time.Time) error {
					if deadline.IsZero() {
						return test.clearErr
					}
					return clientConn.SetReadDeadline(deadline)
				}
			}
			client := New()
			client.conn = wrapped
			err := client.handleHandshakeResponse()
			if test.clearErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, test.clearErr)
			}
			require.NoError(t, <-serverDone)
		})
	}
}

func TestClientHandleHandshakeErrorBranches(t *testing.T) {
	c := New()
	attempt := &connectAttempt{
		ctx:                   context.Background(),
		handshakeData:         c.clientHandshakeData,
		packetEncoder:         c.packetEncoder,
		messageEncoder:        c.messageEncoder,
		cancelledByDisconnect: false,
	}

	readDeadlineErr := errors.New("set read deadline")
	conn := &branchDeadlineConn{
		Conn: newGenerationTestConn(),
		readDeadline: func(time.Time) error {
			return readDeadlineErr
		},
	}
	_, err := c.handleHandshake(attempt, conn)
	require.ErrorIs(t, err, readDeadlineErr)

	encodeErr := errors.New("encode handshake")
	conn = &branchDeadlineConn{Conn: newGenerationTestConn()}
	attempt.packetEncoder = &generationPacketEncoder{err: encodeErr}
	_, err = c.handleHandshake(attempt, conn)
	require.ErrorIs(t, err, encodeErr)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	serverDone := make(chan error, 1)
	go func() { serverDone <- serveClientHandshake(serverConn) }()
	clearErr := errors.New("clear handshake deadline")
	wrapped := &branchDeadlineConn{
		Conn: clientConn,
		readDeadline: func(deadline time.Time) error {
			if deadline.IsZero() {
				return clearErr
			}
			return clientConn.SetReadDeadline(deadline)
		},
	}
	attempt.packetEncoder = c.packetEncoder
	_, err = c.handleHandshake(attempt, wrapped)
	require.ErrorIs(t, err, clearErr)
	require.NoError(t, <-serverDone)
}

func TestClientConnectAdmissionAndRollbackBranches(t *testing.T) {
	c := New()
	_, err := c.connectToContext(context.Background(), "tcp://%zz", nil, nil)
	require.Error(t, err)
	_, err = c.connectToContext(context.Background(), "tls://localhost:1", []*tls.Config{nil}, nil)
	require.Error(t, err)
	_, err = c.connectToContext(nil, "unsupported://localhost", nil, nil)
	require.EqualError(t, err, "unSupport schme:unsupported")

	c.state = clientStateConnected
	_, err = c.connectToContext(context.Background(), "tcp://localhost:1", nil, nil)
	require.ErrorIs(t, err, errClientAlreadyConnected)
	c.state = clientStateConnecting
	_, err = c.connectToContext(context.Background(), "tcp://localhost:1", nil, nil)
	require.ErrorIs(t, err, errClientConnecting)
	c.state = clientStateIdle

	t.Run("context canceled after handshake", func(t *testing.T) {
		listener, serverDone := startBranchHandshakeServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		client := New()
		gen, connectErr := client.connectToContext(ctx, listener.Addr().String(), nil, cancel)
		assert.Nil(t, gen)
		require.ErrorIs(t, connectErr, context.Canceled)
		require.NoError(t, <-serverDone)
		assert.False(t, client.ConnectedStatus())
	})

	t.Run("attempt replaced after handshake", func(t *testing.T) {
		listener, serverDone := startBranchHandshakeServer(t)
		client := New()
		gen, connectErr := client.connectToContext(context.Background(), listener.Addr().String(), nil, func() {
			client.connMutex.Lock()
			client.attempt = nil
			client.state = clientStateIdle
			client.connMutex.Unlock()
		})
		assert.Nil(t, gen)
		require.ErrorIs(t, connectErr, errClientConnectionClosed)
		require.NoError(t, <-serverDone)
		assert.False(t, client.ConnectedStatus())
	})

	expiredAttempt := &connectAttempt{ctx: branchDeadlineContext{deadline: time.Now().Add(-time.Second)}}
	require.ErrorIs(t, c.connectAttemptError(expiredAttempt, branchTimeoutError{}), context.DeadlineExceeded)
}

func TestGenerationPendingAndWriteGuardBranches(t *testing.T) {
	t.Run("closed write", func(t *testing.T) {
		gen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
		close(gen.closeCh)
		_, err := gen.safeWrite([]byte("payload"), time.Second)
		require.ErrorIs(t, err, errClientConnectionClosed)
	})

	t.Run("expiration keeps fresh request", func(t *testing.T) {
		gen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
		fresh := &pendingRequest{msg: &message.Message{ID: 1}, sentAt: time.Now()}
		expired := &pendingRequest{msg: &message.Message{ID: 2}, sentAt: time.Now().Add(-time.Hour)}
		require.NoError(t, gen.registerPending(fresh))
		require.NoError(t, gen.registerPending(expired))
		assert.Equal(t, []*pendingRequest{expired}, gen.takeExpiredPending(time.Now(), time.Minute))
		pending, ok := gen.takePending(1)
		assert.True(t, ok)
		assert.Same(t, fresh, pending)
	})

	t.Run("closed timeout delivery", func(t *testing.T) {
		c := New(time.Millisecond)
		gen := newConnectionGeneration(1, newGenerationTestConn(), c.packetEncoder, c.messageEncoder)
		gen.incomingChan = make(chan *message.Message)
		require.NoError(t, gen.registerPending(&pendingRequest{
			msg:    &message.Message{ID: 1, Route: "test.route"},
			sentAt: time.Now().Add(-time.Second),
		}))
		close(gen.closeCh)
		assert.False(t, c.reapPending(gen, time.Now()))
	})

	t.Run("register closed full generation", func(t *testing.T) {
		gen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
		for range cap(gen.slots) {
			gen.slots <- struct{}{}
		}
		close(gen.closeCh)
		err := gen.registerPending(&pendingRequest{msg: &message.Message{ID: 1}})
		require.ErrorIs(t, err, errClientConnectionClosed)
	})

	t.Run("register closed after slot", func(t *testing.T) {
		gen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
		gen.closed = true
		err := gen.registerPending(&pendingRequest{msg: &message.Message{ID: 1}})
		require.ErrorIs(t, err, errClientConnectionClosed)
		assert.Empty(t, gen.slots)
	})

	t.Run("duplicate request", func(t *testing.T) {
		gen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
		pending := &pendingRequest{msg: &message.Message{ID: 9}}
		require.NoError(t, gen.registerPending(pending))
		err := gen.registerPending(&pendingRequest{msg: &message.Message{ID: 9}})
		require.EqualError(t, err, "client: duplicate request id 9")
		got, ok := gen.takePending(9)
		assert.True(t, ok)
		assert.Same(t, pending, got)
	})

	c := New()
	_, err := c.SendRequest("test.route", nil)
	require.ErrorIs(t, err, errClientConnectionClosed)
	_, err = c.SafeWrite(nil)
	require.ErrorIs(t, err, errClientConnectionClosed)

	gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
	gen.closed = true
	_, err = c.SendRequest("test.route", nil)
	require.ErrorIs(t, err, errClientConnectionClosed)
	c.Disconnect(CloseReasonManual)
}

func TestGenerationLoopExitBranches(t *testing.T) {
	t.Run("heartbeat generation closes", func(t *testing.T) {
		c := New()
		gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
		c.startGenerationLoops(gen, 1)
		c.Disconnect(CloseReasonManual)
		waitForGenerationLoops(t, gen)
	})

	t.Run("kick", func(t *testing.T) {
		c := New()
		gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
		done := make(chan struct{})
		go func() {
			c.handlePackets(gen)
			close(done)
		}()
		gen.packetChan <- &packet.Packet{Type: packet.Kick}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("kick did not stop packet loop")
		}
		select {
		case <-gen.closeCh:
		default:
			t.Fatal("kick did not close generation")
		}
	})

	t.Run("blocked delivery closes", func(t *testing.T) {
		c := New()
		gen := newConnectionGeneration(1, newGenerationTestConn(), c.packetEncoder, c.messageEncoder)
		gen.packetChan = make(chan *packet.Packet)
		gen.incomingChan = make(chan *message.Message)
		encoded, err := message.NewMessagesEncoder(false).Encode(&message.Message{Type: message.Push, Route: "test.push"})
		require.NoError(t, err)
		done := make(chan struct{})
		go func() {
			c.handlePackets(gen)
			close(done)
		}()
		gen.packetChan <- &packet.Packet{Type: packet.Data, Data: encoded}
		close(gen.closeCh)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("closed delivery did not stop packet loop")
		}
	})

	t.Run("server reader closes while forwarding", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		c := New()
		gen := newConnectionGeneration(1, clientConn, c.packetEncoder, c.messageEncoder)
		gen.packetChan = make(chan *packet.Packet)
		done := make(chan struct{})
		go func() {
			c.handleServerMessages(gen)
			close(done)
		}()
		frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Heartbeat, nil)
		require.NoError(t, err)
		_, err = serverConn.Write(frame)
		require.NoError(t, err)
		c.disconnectGeneration(gen, CloseReasonManual)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("closed generation did not stop server reader")
		}
	})

	t.Run("heartbeat write", func(t *testing.T) {
		c := New()
		conn := newGenerationTestConn()
		wrote := make(chan struct{})
		var once sync.Once
		conn.write = func(data []byte) (int, error) {
			once.Do(func() { close(wrote) })
			return len(data), nil
		}
		gen := newConnectionGeneration(1, conn, c.packetEncoder, c.messageEncoder)
		gen.recordServerActivity(time.Now())
		done := make(chan struct{})
		go func() {
			c.sendHeartbeats(gen, 1)
			close(done)
		}()
		select {
		case <-wrote:
		case <-time.After(2 * time.Second):
			t.Fatal("heartbeat was not written")
		}
		c.disconnectGeneration(gen, CloseReasonManual)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("heartbeat loop did not stop")
		}
	})
}

func TestHandleServerMessagesClosedReadIsQuiet(t *testing.T) {
	c := New()
	conn := newGenerationTestConn()
	gen := newConnectionGeneration(1, conn, c.packetEncoder, c.messageEncoder)
	c.disconnectGeneration(gen, CloseReasonManual)
	c.handleServerMessages(gen)
	_, err := conn.Read(nil)
	require.ErrorIs(t, err, io.EOF)
}
