package client

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/stretchr/testify/require"
)

func TestQueuedRequestTimeoutStartsAtWrite(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := New(time.Second)
			gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
			defer c.Disconnect(CloseReasonManual)
			for range clientInflightLimit {
				_, err := c.SendRequest("queued", nil)
				require.NoError(t, err)
			}
			var id uint
			var err error
			go func() { id, err = c.SendRequest("fresh", nil) }()
			synctest.Wait()
			time.Sleep(10 * time.Second)
			_, ok := gen.takePending(1)
			require.True(t, ok)
			synctest.Wait()
			require.NoError(t, err)
			for _, expired := range gen.takeExpiredPending(time.Now(), c.requestTimeout) {
				require.NotEqual(t, id, expired.msg.ID)
			}
			pending, ok := gen.takePending(id)
			require.True(t, ok)
			require.Equal(t, time.Now(), pending.sentAt)
		})
	})
	t.Run("writer", func(t *testing.T) {
		c := New(time.Second)
		gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
		defer c.Disconnect(CloseReasonManual)
		gen.writeMu.Lock()
		done := make(chan error, 1)
		go func() { _, err := c.SendRequest("fresh", nil); done <- err }()
		require.Eventually(t, func() bool { n, _ := pendingState(gen); return n == 1 }, time.Second, time.Millisecond)
		expired := gen.takeExpiredPending(time.Now().Add(time.Hour), c.requestTimeout)
		released := time.Now()
		gen.writeMu.Unlock()
		require.NoError(t, <-done)
		require.Empty(t, expired, "unsent request expired while waiting for writer")
		pending, ok := gen.takePending(1)
		require.True(t, ok)
		require.False(t, pending.sentAt.Before(released))
	})
}

func TestConcurrentSendDisconnectAndStatus(t *testing.T) {
	c := New()
	local, remote := net.Pipe()
	defer remote.Close()
	gen := installTestGeneration(c, local, false, 0)
	var callbacks atomic.Int64
	c.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	result := make(chan error, 1)
	go func() { _, err := c.SendRequest("test", nil); result <- err }()
	// Reading only the header proves a write is active and still blocked on its body.
	header := make([]byte, codec.HeadLength)
	_, err := io.ReadFull(remote, header)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { c.ConnectedStatus(); c.MsgChannel(); c.Disconnect(CloseReasonManual) })
	}
	wg.Wait()
	require.Error(t, <-result)
	require.Equal(t, int64(1), callbacks.Load())
	pending, slots := pendingState(gen)
	require.Zero(t, pending)
	require.Zero(t, slots)
	require.False(t, c.ConnectedStatus())
}

func TestResponseAndTimeoutCompleteOnce(t *testing.T) {
	c := New(time.Second)
	gen := installTestGeneration(c, newGenerationTestConn(), false, 0)
	defer c.Disconnect(CloseReasonManual)
	for id := uint(1); id <= 100; id++ {
		require.NoError(t, gen.registerPending(&pendingRequest{msg: &message.Message{ID: id}, sentAt: time.Now().Add(-time.Minute)}))
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, ok := gen.takePending(id); ok {
				gen.deliver(&message.Message{ID: id})
			}
		})
		wg.Go(func() { c.reapPending(gen, time.Now()) })
		wg.Wait()
		select {
		case got := <-gen.incomingChan:
			require.Equal(t, id, got.ID)
		default:
			t.Fatal("missing completion")
		}
		select {
		case <-gen.incomingChan:
			t.Fatal("duplicate completion")
		default:
		}
	}
}

func TestConnectPreservesCoalescedFirstData(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if _, err = readPacket(conn); err != nil {
			done <- err
			return
		}
		enc := codec.NewPomeloPacketEncoder()
		hs, _ := enc.Encode(packet.Handshake, []byte(`{"code":200,"sys":{"heartbeat":0}}`))
		msg, _ := message.NewMessagesEncoder(false).Encode(&message.Message{Type: message.Push, Route: "first", Data: []byte("hello")})
		push, _ := enc.Encode(packet.Data, msg)
		if _, err = conn.Write(append(hs, push...)); err != nil {
			done <- err
			return
		}
		ack, err := readPacket(conn)
		if err == nil && ack.Type != packet.HandshakeAck {
			err = errors.New("missing ack")
		}
		if err != nil {
			done <- err
			return
		}
		_, err = readPacket(conn)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		done <- err
	}()
	c := New()
	require.NoError(t, c.ConnectTo(listener.Addr().String()))
	select {
	case m := <-c.MsgChannel():
		require.Equal(t, "first", m.Route)
		require.Equal(t, []byte("hello"), m.Data)
	case <-time.After(time.Second):
		t.Fatal("first data lost")
	}
	require.ErrorIs(t, c.ConnectTo("tcp://127.0.0.1:1"), errClientAlreadyConnected)
	require.True(t, c.ConnectedStatus())
	c.Disconnect(CloseReasonManual)
	require.NoError(t, <-done)
}
