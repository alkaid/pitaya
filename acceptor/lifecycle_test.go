package acceptor

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const acceptorTestTimeout = 3 * time.Second

func serveForTest(t *testing.T, a Acceptor, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- a.Serve(ctx)
	}()

	select {
	case err := <-a.Ready():
		require.NoError(t, err)
	case <-time.After(acceptorTestTimeout):
		t.Fatal("acceptor did not publish readiness")
	}
	return done
}

func requireServeStopped(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(acceptorTestTimeout):
		t.Fatal("Serve did not return after Stop")
	}
}

type queuedListenerAddr struct{}

func (queuedListenerAddr) Network() string { return "test" }
func (queuedListenerAddr) String() string  { return "queued-listener" }

type queuedListener struct {
	connections chan net.Conn
	acceptCalls chan int64
	closed      chan struct{}
	closeOnce   sync.Once
	acceptCount atomic.Int64
}

func newQueuedListener(capacity int) *queuedListener {
	return &queuedListener{
		connections: make(chan net.Conn, capacity),
		acceptCalls: make(chan int64, capacity),
		closed:      make(chan struct{}),
	}
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		count := l.acceptCount.Add(1)
		l.acceptCalls <- count
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *queuedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *queuedListener) Addr() net.Addr { return queuedListenerAddr{} }

type stagedCloseListener struct {
	acceptStarted chan struct{}
	closeStarted  chan struct{}
	releaseClose  chan struct{}
	closed        chan struct{}
	acceptErr     error
}

func newStagedCloseListener(acceptErr error) *stagedCloseListener {
	return &stagedCloseListener{
		acceptStarted: make(chan struct{}),
		closeStarted:  make(chan struct{}),
		releaseClose:  make(chan struct{}),
		closed:        make(chan struct{}),
		acceptErr:     acceptErr,
	}
}

func (l *stagedCloseListener) Accept() (net.Conn, error) {
	close(l.acceptStarted)
	<-l.closed
	return nil, l.acceptErr
}

func (l *stagedCloseListener) Close() error {
	close(l.closeStarted)
	<-l.releaseClose
	close(l.closed)
	return nil
}

func (l *stagedCloseListener) Addr() net.Addr { return queuedListenerAddr{} }

type blockedErrorListener struct {
	acceptStarted chan struct{}
	releaseAccept chan struct{}
	acceptErr     error
}

func (l *blockedErrorListener) Accept() (net.Conn, error) {
	close(l.acceptStarted)
	<-l.releaseAccept
	return nil, l.acceptErr
}

func (l *blockedErrorListener) Close() error   { return nil }
func (l *blockedErrorListener) Addr() net.Addr { return queuedListenerAddr{} }

func TestAcceptorStopBeforeServeIsSafeAndIdempotent(t *testing.T) {
	tests := []struct {
		name string
		new  func() Acceptor
	}{
		{name: "tcp", new: func() Acceptor { return NewTCPAcceptor("127.0.0.1:0") }},
		{name: "websocket", new: func() Acceptor { return NewWSAcceptor("127.0.0.1:0") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.new()
			var wg sync.WaitGroup
			for range 32 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					a.Stop()
				}()
			}
			wg.Wait()

			select {
			case err := <-a.Ready():
				require.ErrorIs(t, err, ErrAcceptorStopped)
			case <-time.After(time.Second):
				t.Fatal("Stop did not resolve Ready")
			}
			require.ErrorIs(t, a.Serve(context.Background()), ErrAcceptorStopped)
			assert.False(t, a.IsRunning())
			assert.Empty(t, a.GetAddr())
		})
	}
}

func TestAcceptorConcurrentStopAndStatusAccess(t *testing.T) {
	tests := []struct {
		name string
		new  func() Acceptor
	}{
		{name: "tcp", new: func() Acceptor { return NewTCPAcceptor("127.0.0.1:0") }},
		{name: "websocket", new: func() Acceptor { return NewWSAcceptor("127.0.0.1:0") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.new()
			done := serveForTest(t, a, context.Background())
			require.NotEmpty(t, a.GetAddr())

			readersDone := make(chan struct{})
			var readers sync.WaitGroup
			for range 8 {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for {
						select {
						case <-readersDone:
							return
						default:
							_ = a.GetAddr()
							_ = a.IsRunning()
						}
					}
				}()
			}

			var stoppers sync.WaitGroup
			for range 32 {
				stoppers.Add(1)
				go func() {
					defer stoppers.Done()
					a.Stop()
				}()
			}
			stoppers.Wait()
			close(readersDone)
			readers.Wait()
			requireServeStopped(t, done)
			assert.False(t, a.IsRunning())
		})
	}
}

func TestAcceptorReadyReportsBindError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()

	tests := []struct {
		name string
		new  func() Acceptor
	}{
		{name: "tcp", new: func() Acceptor { return NewTCPAcceptor(occupied.Addr().String()) }},
		{name: "websocket", new: func() Acceptor { return NewWSAcceptor(occupied.Addr().String()) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.new()
			done := make(chan error, 1)
			go func() { done <- a.Serve(context.Background()) }()

			select {
			case readyErr := <-a.Ready():
				require.Error(t, readyErr)
			case <-time.After(acceptorTestTimeout):
				t.Fatal("bind failure was not published")
			}
			select {
			case serveErr := <-done:
				require.Error(t, serveErr)
			case <-time.After(acceptorTestTimeout):
				t.Fatal("Serve did not return bind failure")
			}
			assert.False(t, a.IsRunning())
		})
	}
}

func TestAcceptorReadyBroadcastsAndReplaysResult(t *testing.T) {
	tests := []struct {
		name string
		new  func() Acceptor
	}{
		{name: "tcp", new: func() Acceptor { return NewTCPAcceptor("127.0.0.1:0") }},
		{name: "websocket", new: func() Acceptor { return NewWSAcceptor("127.0.0.1:0") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.new()
			first := a.Ready()
			second := a.Ready()
			done := make(chan error, 1)
			go func() { done <- a.Serve(context.Background()) }()

			require.NoError(t, <-first)
			require.NoError(t, <-second)
			require.NoError(t, <-a.Ready())

			a.Stop()
			requireServeStopped(t, done)
		})
	}
}

func TestProxyProtocolDisabledAcceptsDirectlyFromUnderlyingListener(t *testing.T) {
	connectionCount := DefaultProxyProtocolMaxPending + 2
	listener := newQueuedListener(connectionCount)
	servers := make([]net.Conn, 0, connectionCount)
	peers := make([]net.Conn, 0, connectionCount)
	for range connectionCount {
		server, peer := net.Pipe()
		servers = append(servers, server)
		peers = append(peers, peer)
		listener.connections <- server
	}

	wrapped := &ProxyProtocolListener{Listener: listener}
	defer func() {
		_ = wrapped.Close()
		for _, conn := range servers {
			_ = conn.Close()
		}
		for _, conn := range peers {
			_ = conn.Close()
		}
	}()

	accepted, err := wrapped.Accept()
	require.NoError(t, err)
	defer accepted.Close()
	require.Equal(t, int64(1), <-listener.acceptCalls)

	select {
	case count := <-listener.acceptCalls:
		assert.Failf(t, "unexpected pre-accept", "underlying Accept was called %d times", count)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Equal(t, int64(1), listener.acceptCount.Load())

	lastPeer := peers[len(peers)-1]
	require.NoError(t, lastPeer.SetReadDeadline(time.Now().Add(50*time.Millisecond)))
	_, err = lastPeer.Read(make([]byte, 1))
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	assert.True(t, netErr.Timeout(), "queued connection was actively closed: %v", err)
}

func TestProxyProtocolCloseDoesNotLeakUnderlyingAcceptError(t *testing.T) {
	underlyingErr := errors.New("synthetic listener close")
	for range 64 {
		listener := newStagedCloseListener(underlyingErr)
		wrapped := &ProxyProtocolListener{
			Listener:             listener,
			ProxyProtocolEnabled: true,
		}

		acceptDone := make(chan error, 1)
		go func() {
			_, err := wrapped.Accept()
			acceptDone <- err
		}()
		select {
		case <-listener.acceptStarted:
		case <-time.After(acceptorTestTimeout):
			t.Fatal("underlying Accept did not start")
		}

		closeDone := make(chan error, 1)
		go func() { closeDone <- wrapped.Close() }()
		select {
		case <-listener.closeStarted:
		case <-time.After(acceptorTestTimeout):
			t.Fatal("underlying Close did not start")
		}
		close(listener.releaseClose)

		select {
		case err := <-acceptDone:
			require.ErrorIs(t, err, net.ErrClosed)
			require.NotErrorIs(t, err, underlyingErr)
		case <-time.After(acceptorTestTimeout):
			t.Fatal("wrapped Accept did not return after Close")
		}
		select {
		case err := <-closeDone:
			require.NoError(t, err)
		case <-time.After(acceptorTestTimeout):
			t.Fatal("wrapped Close did not return")
		}
	}
}

func TestProxyProtocolAcceptForwardsUnderlyingListenerError(t *testing.T) {
	listener := newQueuedListener(0)
	require.NoError(t, listener.Close())
	wrapper := &ProxyProtocolListener{
		Listener:             listener,
		ProxyProtocolEnabled: true,
	}
	t.Cleanup(func() { require.NoError(t, wrapper.Close()) })

	_, err := wrapper.Accept()
	require.ErrorIs(t, err, net.ErrClosed)
}

func TestProxyProtocolCloseUnblocksPendingListenerError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener := &blockedErrorListener{
			acceptStarted: make(chan struct{}),
			releaseAccept: make(chan struct{}),
			acceptErr:     errors.New("synthetic listener failure"),
		}
		wrapper := &ProxyProtocolListener{
			Listener:             listener,
			ProxyProtocolEnabled: true,
		}

		wrapper.startPump()
		<-listener.acceptStarted
		close(listener.releaseAccept)
		synctest.Wait()

		require.NoError(t, wrapper.Close())
		_, ok := <-wrapper.results
		require.False(t, ok)
	})
}

func TestTCPProxyHeaderTimeoutRejectsAndContinues(t *testing.T) {
	tests := []struct {
		name    string
		partial []byte
	}{
		{name: "no bytes"},
		{name: "partial header", partial: []byte("PROXY TCP4 ")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewTCPAcceptor("127.0.0.1:0")
			a.proxyProtocolHeaderTimeout = 100 * time.Millisecond
			a.EnableProxyProtocol()
			done := serveForTest(t, a, context.Background())
			defer func() {
				a.Stop()
				requireServeStopped(t, done)
			}()

			slow, err := net.Dial("tcp", a.GetAddr())
			require.NoError(t, err)
			defer slow.Close()
			if len(tt.partial) > 0 {
				_, err = slow.Write(tt.partial)
				require.NoError(t, err)
			}
			time.Sleep(20 * time.Millisecond)

			good, err := net.Dial("tcp", a.GetAddr())
			require.NoError(t, err)
			defer good.Close()
			_, err = good.Write([]byte("PROXY TCP4 192.0.2.10 192.0.2.20 12345 443\r\n"))
			require.NoError(t, err)

			var player PlayerConn
			select {
			case player = <-a.GetConnChan():
			case <-time.After(acceptorTestTimeout):
				t.Fatal("valid connection was not accepted after slow PROXY peer")
			}
			defer player.Close()
			assert.Equal(t, "192.0.2.10:12345", player.RemoteAddr().String())

			// The PROXY-only deadline must not leak into normal packet reads.
			time.Sleep(150 * time.Millisecond)
			packet := []byte{0x02, 0x00, 0x00, 0x01, 0x00}
			_, err = good.Write(packet)
			require.NoError(t, err)
			message, err := player.GetNextMessage()
			require.NoError(t, err)
			assert.Equal(t, packet, message)

			require.NoError(t, slow.SetReadDeadline(time.Now().Add(time.Second)))
			_, err = slow.Read(make([]byte, 1))
			require.Error(t, err)
		})
	}
}

func TestTCPProxySlowHeaderDoesNotBlockAcceptAndStop(t *testing.T) {
	a := NewTCPAcceptor("127.0.0.1:0")
	a.proxyProtocolHeaderTimeout = 2 * time.Second
	a.EnableProxyProtocol()
	done := serveForTest(t, a, context.Background())

	slow, err := net.Dial("tcp", a.GetAddr())
	require.NoError(t, err)
	defer slow.Close()

	good, err := net.Dial("tcp", a.GetAddr())
	require.NoError(t, err)
	defer good.Close()
	_, err = good.Write([]byte("PROXY TCP4 192.0.2.10 192.0.2.20 12345 443\r\n"))
	require.NoError(t, err)

	var player PlayerConn
	select {
	case player = <-a.GetConnChan():
		defer player.Close()
	case <-time.After(500 * time.Millisecond):
		a.Stop()
		t.Fatal("valid PROXY peer was blocked behind a slow header")
	}

	stopStarted := time.Now()
	a.Stop()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop waited for the PROXY header deadline")
	}
	assert.Less(t, time.Since(stopStarted), a.proxyProtocolHeaderTimeout)
}

func TestTCPProxyParserAdmissionIsBounded(t *testing.T) {
	a := NewTCPAcceptor("127.0.0.1:0")
	a.proxyProtocolHeaderTimeout = 10 * time.Second
	a.EnableProxyProtocol()
	done := serveForTest(t, a, context.Background())

	connections := make([]net.Conn, 0, DefaultProxyProtocolMaxPending)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for range DefaultProxyProtocolMaxPending {
		conn, err := net.Dial("tcp", a.GetAddr())
		require.NoError(t, err)
		connections = append(connections, conn)
	}

	a.lifecycle.mu.RLock()
	listener, ok := a.lifecycle.listener.(*ProxyProtocolListener)
	a.lifecycle.mu.RUnlock()
	require.True(t, ok)
	require.Eventually(t, func() bool {
		listener.pendingMu.Lock()
		defer listener.pendingMu.Unlock()
		return len(listener.pending) == DefaultProxyProtocolMaxPending
	}, time.Second, 10*time.Millisecond)

	overflow, err := net.Dial("tcp", a.GetAddr())
	require.NoError(t, err)
	defer overflow.Close()
	require.NoError(t, overflow.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = overflow.Read(make([]byte, 1))
	require.Error(t, err)

	a.Stop()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop did not close saturated PROXY parsers")
	}
}

func TestWSServePublishesReady(t *testing.T) {
	w := NewWSAcceptor("127.0.0.1:0", "./fixtures/server.crt", "./fixtures/server.key")
	done := serveForTest(t, w, context.Background())
	defer func() {
		w.Stop()
		requireServeStopped(t, done)
	}()

	dialer := *websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec -- test certificate
	conn, _, err := dialer.Dial("wss://"+w.GetAddr(), nil)
	require.NoError(t, err)
	defer conn.Close()
	w.serverMu.Lock()
	server := w.server
	w.serverMu.Unlock()
	require.NotNil(t, server)
	assert.Equal(t, DefaultWSReadHeaderTimeout, server.ReadHeaderTimeout)
	assert.Equal(t, DefaultWSIdleTimeout, server.IdleTimeout)
	assert.Equal(t, DefaultWSMaxHeaderBytes, server.MaxHeaderBytes)

	select {
	case accepted := <-w.GetConnChan():
		require.NotNil(t, accepted)
		defer accepted.Close()
	case <-time.After(acceptorTestTimeout):
		t.Fatal("WSS connection was not accepted after readiness")
	}
	assert.True(t, w.IsRunning())
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestWSReadLimitRejectsOversizedMessage(t *testing.T) {
	w := NewWSAcceptor("127.0.0.1:0")
	done := serveForTest(t, w, context.Background())
	defer func() {
		w.Stop()
		requireServeStopped(t, done)
	}()

	client, _, err := websocket.DefaultDialer.Dial("ws://"+w.GetAddr(), nil)
	require.NoError(t, err)
	defer client.Close()

	var player *WSConn
	select {
	case accepted := <-w.GetConnChan():
		player = accepted.(*WSConn)
	case <-time.After(acceptorTestTimeout):
		t.Fatal("WebSocket connection was not accepted")
	}
	defer player.Close()

	writeDone := make(chan error, 1)
	go func() {
		writer, writeErr := client.NextWriter(websocket.BinaryMessage)
		if writeErr == nil {
			_, writeErr = io.CopyN(writer, zeroReader{}, int64(codec.HeadLength+codec.MaxPacketSize+1))
			if closeErr := writer.Close(); writeErr == nil {
				writeErr = closeErr
			}
		}
		writeDone <- writeErr
	}()

	_, reader, err := player.InnerConn().NextReader()
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, reader)
	require.ErrorIs(t, err, websocket.ErrReadLimit)
	_ = player.Close()

	select {
	case <-writeDone:
	case <-time.After(acceptorTestTimeout):
		t.Fatal("oversized WebSocket writer did not unblock")
	}
}

func TestWSOriginPolicyDefaultsToSameOriginAndAllowsOptIn(t *testing.T) {
	headers := http.Header{"Origin": []string{"https://cross-origin.example"}}

	w := NewWSAcceptor("127.0.0.1:0")
	done := serveForTest(t, w, context.Background())
	_, response, err := websocket.DefaultDialer.Dial("ws://"+w.GetAddr(), headers)
	require.Error(t, err)
	if response != nil {
		assert.Equal(t, http.StatusForbidden, response.StatusCode)
		_ = response.Body.Close()
	}
	w.Stop()
	requireServeStopped(t, done)

	w = NewWSAcceptor("127.0.0.1:0")
	w.SetCheckOrigin(func(*http.Request) bool { return true })
	done = serveForTest(t, w, context.Background())
	defer func() {
		w.Stop()
		requireServeStopped(t, done)
	}()
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+w.GetAddr(), headers)
	require.NoError(t, err)
	defer conn.Close()

	select {
	case accepted := <-w.GetConnChan():
		defer accepted.Close()
	case <-time.After(acceptorTestTimeout):
		t.Fatal("opted-in cross-origin connection was not accepted")
	}
}

func TestWSSameOriginRequiresMatchingTransportScheme(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		origins []string
		want    bool
	}{
		{name: "non-browser", target: "http://game.example/socket", want: true},
		{name: "ws", target: "http://game.example/socket", origins: []string{"http://game.example"}, want: true},
		{name: "wss", target: "https://game.example/socket", origins: []string{"https://game.example"}, want: true},
		{name: "https-origin-over-ws", target: "http://game.example/socket", origins: []string{"https://game.example"}},
		{name: "http-origin-over-wss", target: "https://game.example/socket", origins: []string{"http://game.example"}},
		{name: "cross-host", target: "https://game.example/socket", origins: []string{"https://other.example"}},
		{name: "multiple-origins", target: "https://game.example/socket", origins: []string{"https://game.example", "https://game.example"}},
		{name: "missing-scheme", target: "https://game.example/socket", origins: []string{"//game.example"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			for _, origin := range tt.origins {
				req.Header.Add("Origin", origin)
			}
			assert.Equal(t, tt.want, checkSameOrigin(req))
		})
	}
}

func TestServeContextCancellationIsClean(t *testing.T) {
	tests := []struct {
		name string
		new  func() Acceptor
	}{
		{name: "tcp", new: func() Acceptor { return NewTCPAcceptor("127.0.0.1:0") }},
		{name: "websocket", new: func() Acceptor { return NewWSAcceptor("127.0.0.1:0") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			a := tt.new()
			done := serveForTest(t, a, ctx)
			cancel()
			requireServeStopped(t, done)
			assert.False(t, a.IsRunning())
		})
	}
}

func TestNilServeContextConsumesOnlyTheFirstStartAttempt(t *testing.T) {
	tests := []struct {
		name string
		new  func() Acceptor
	}{
		{name: "tcp", new: func() Acceptor { return NewTCPAcceptor("127.0.0.1:0") }},
		{name: "websocket", new: func() Acceptor { return NewWSAcceptor("127.0.0.1:0") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.new()
			require.ErrorIs(t, a.Serve(nil), ErrNilServeContext)
			require.ErrorIs(t, <-a.Ready(), ErrNilServeContext)
			require.ErrorIs(t, a.Serve(context.Background()), ErrAcceptorStopped)
			assert.False(t, a.IsRunning())
		})
	}
}

func TestRepeatedInvalidStartDoesNotStopRunningAcceptor(t *testing.T) {
	tests := []struct {
		name     string
		new      func() Acceptor
		startTLS func(Acceptor) error
	}{
		{
			name: "tcp",
			new:  func() Acceptor { return NewTCPAcceptor("127.0.0.1:0") },
			startTLS: func(a Acceptor) error {
				return a.(*TCPAcceptor).ListenAndServeTLS("missing.crt", "missing.key")
			},
		},
		{
			name: "websocket",
			new:  func() Acceptor { return NewWSAcceptor("127.0.0.1:0") },
			startTLS: func(a Acceptor) error {
				return a.(*WSAcceptor).ListenAndServeTLS("missing.crt", "missing.key")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.new()
			done := serveForTest(t, a, context.Background())
			defer func() {
				a.Stop()
				requireServeStopped(t, done)
			}()

			require.ErrorIs(t, a.Serve(nil), ErrAcceptorAlreadyStarted)
			require.ErrorIs(t, tt.startTLS(a), ErrAcceptorAlreadyStarted)
			assert.True(t, a.IsRunning())

			conn, err := net.DialTimeout("tcp", a.GetAddr(), time.Second)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
		})
	}
}

func TestWSInvalidTLSIsReportedByReadyAndServe(t *testing.T) {
	w := NewWSAcceptor("127.0.0.1:0", "missing.crt", "missing.key")
	done := make(chan error, 1)
	go func() { done <- w.Serve(context.Background()) }()

	readyErr := <-w.Ready()
	require.Error(t, readyErr)
	serveErr := <-done
	require.Error(t, serveErr)
	assert.EqualError(t, serveErr, readyErr.Error())
	assert.Contains(t, serveErr.Error(), "invalid certificates")
}
