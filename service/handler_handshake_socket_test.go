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

package service

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/agent"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/pipeline"
	serializejson "github.com/alkaid/pitaya/v2/serialize/json"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/require"
)

const strictHandshakeSocketTimeout = 2 * time.Second

type strictHandshakeSocketHarness struct {
	acceptor   *acceptor.TCPAcceptor
	encoder    *codec.PomeloPacketEncoder
	sessions   session.SessionPool
	cancel     context.CancelFunc
	serveDone  chan error
	readerDone chan struct{}
	handlers   sync.WaitGroup
}

func newStrictHandshakeSocketHarness(t *testing.T) *strictHandshakeSocketHarness {
	t.Helper()

	decoder := codec.NewPomeloPacketDecoder()
	encoder := codec.NewPomeloPacketEncoder()
	serializer := serializejson.NewSerializer()
	sessions := session.NewSessionPool()
	// Agent cache keepalive needs a configured cache. The long TTL ensures these
	// short socket tests never issue a Redis command against the nil test client.
	sessions.SetClusterCache(session.NewRedisCache(nil, time.Hour))

	agentFactory := agent.NewAgentFactory(
		nil,
		decoder,
		encoder,
		serializer,
		10*time.Second,
		message.NewMessagesEncoder(false),
		16,
		sessions,
		nil,
		"strict-handshake-frontend",
	)
	handler := NewHandlerService(
		decoder,
		serializer,
		1,
		1,
		cluster.NewServer("strict-handshake-frontend", "frontend", true),
		nil,
		agentFactory,
		nil,
		pipeline.NewHandlerHooks(),
		NewHandlerPool(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	listener := acceptor.NewTCPAcceptor("127.0.0.1:0")
	h := &strictHandshakeSocketHarness{
		acceptor:   listener,
		encoder:    encoder,
		sessions:   sessions,
		cancel:     cancel,
		serveDone:  make(chan error, 1),
		readerDone: make(chan struct{}),
	}

	go func() {
		defer close(h.readerDone)
		for {
			select {
			case conn := <-listener.GetConnChan():
				h.handlers.Add(1)
				go func() {
					defer h.handlers.Done()
					handler.Handle(conn)
				}()
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		h.serveDone <- listener.Serve(ctx)
	}()

	t.Cleanup(func() {
		h.cancel()
		h.acceptor.Stop()
		select {
		case err := <-h.serveDone:
			if err != nil {
				t.Errorf("TCP acceptor stopped with error: %v", err)
			}
		case <-time.After(strictHandshakeSocketTimeout):
			t.Error("timed out waiting for TCP acceptor shutdown")
		}
		select {
		case <-h.readerDone:
		case <-time.After(strictHandshakeSocketTimeout):
			t.Error("timed out waiting for acceptor connection reader shutdown")
		}

		h.sessions.CloseAll()
		handlersDone := make(chan struct{})
		go func() {
			h.handlers.Wait()
			close(handlersDone)
		}()
		select {
		case <-handlersDone:
		case <-time.After(strictHandshakeSocketTimeout):
			t.Error("timed out waiting for connection handlers to stop")
		}
	})

	select {
	case err := <-listener.Ready():
		require.NoError(t, err)
	case <-time.After(strictHandshakeSocketTimeout):
		t.Fatal("timed out waiting for TCP acceptor readiness")
	}

	return h
}

func (h *strictHandshakeSocketHarness) dial(t *testing.T) net.Conn {
	t.Helper()
	requireStrictHandshakeSessionCount(t, h.sessions, 0)

	conn, err := net.DialTimeout("tcp", h.acceptor.GetAddr(), strictHandshakeSocketTimeout)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	requireStrictHandshakeSessionCount(t, h.sessions, 1)
	return conn
}

func (h *strictHandshakeSocketHarness) writePacket(t *testing.T, conn net.Conn, typ packet.Type, data []byte) {
	t.Helper()
	encoded, err := h.encoder.Encode(typ, data)
	require.NoError(t, err)
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(strictHandshakeSocketTimeout)))
	written, err := conn.Write(encoded)
	require.NoError(t, err)
	require.Equal(t, len(encoded), written)
}

func readStrictHandshakePacket(t *testing.T, conn net.Conn) (packet.Type, []byte) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(strictHandshakeSocketTimeout)))

	header := make([]byte, codec.HeadLength)
	_, err := io.ReadFull(conn, header)
	require.NoError(t, err)
	size, typ, err := codec.ParseHeader(header)
	require.NoError(t, err)
	payload := make([]byte, size)
	_, err = io.ReadFull(conn, payload)
	require.NoError(t, err)
	return typ, payload
}

func requireStrictHandshakeServerClose(t *testing.T, conn net.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(strictHandshakeSocketTimeout)))

	buffer := make([]byte, 1)
	n, err := conn.Read(buffer)
	require.Error(t, err, "server kept an invalid handshake connection open")
	require.Zero(t, n)
	if netErr, ok := err.(net.Error); ok {
		require.False(t, netErr.Timeout(), "timed out instead of observing a server-side close")
	}
}

func requireStrictHandshakeSessionCount(t *testing.T, sessions session.SessionPool, expected int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return sessions.GetSessionCount() == expected
	}, strictHandshakeSocketTimeout, 10*time.Millisecond, "session count did not become %d; current count is %d", expected, sessions.GetSessionCount())
}

func completeStrictHandshake(t *testing.T, h *strictHandshakeSocketHarness, conn net.Conn) {
	t.Helper()
	h.writePacket(t, conn, packet.Handshake, []byte(`{"sys":{"platform":"socket-test"}}`))
	typ, payload := readStrictHandshakePacket(t, conn)
	require.Equal(t, packet.Type(packet.Handshake), typ)
	require.NotEmpty(t, payload)
	h.writePacket(t, conn, packet.HandshakeAck, nil)
}

func TestStrictHandshakeFSMOverTCPSocket(t *testing.T) {
	h := newStrictHandshakeSocketHarness(t)

	for _, test := range []struct {
		name string
		typ  packet.Type
		data []byte
	}{
		{name: "ack_first", typ: packet.HandshakeAck},
		{name: "data_first", typ: packet.Data},
		{name: "heartbeat_first", typ: packet.Heartbeat},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			conn := h.dial(t)
			h.writePacket(t, conn, test.typ, test.data)
			requireStrictHandshakeServerClose(t, conn)
			requireStrictHandshakeSessionCount(t, h.sessions, 0)
		})
	}

	t.Run("duplicate_handshake", func(t *testing.T) {
		conn := h.dial(t)
		h.writePacket(t, conn, packet.Handshake, []byte(`{"sys":{"platform":"socket-test"}}`))
		typ, payload := readStrictHandshakePacket(t, conn)
		require.Equal(t, packet.Type(packet.Handshake), typ)
		require.NotEmpty(t, payload)

		h.writePacket(t, conn, packet.Handshake, []byte(`{"sys":{"platform":"socket-test"}}`))
		requireStrictHandshakeServerClose(t, conn)
		requireStrictHandshakeSessionCount(t, h.sessions, 0)
	})

	t.Run("duplicate_ack_after_handshake", func(t *testing.T) {
		conn := h.dial(t)
		completeStrictHandshake(t, h, conn)
		h.writePacket(t, conn, packet.HandshakeAck, nil)
		requireStrictHandshakeServerClose(t, conn)
		requireStrictHandshakeSessionCount(t, h.sessions, 0)
	})

	t.Run("working_connection_remains_usable", func(t *testing.T) {
		conn := h.dial(t)
		completeStrictHandshake(t, h, conn)

		h.writePacket(t, conn, packet.Heartbeat, nil)
		typ, payload := readStrictHandshakePacket(t, conn)
		require.Equal(t, packet.Type(packet.HeartbeatAck), typ)
		require.Empty(t, payload)
		requireStrictHandshakeSessionCount(t, h.sessions, 1)

		require.NoError(t, conn.Close())
		requireStrictHandshakeSessionCount(t, h.sessions, 0)
	})
}
