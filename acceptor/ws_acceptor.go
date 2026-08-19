// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package acceptor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/gorilla/websocket"
)

const (
	DefaultWSReadHeaderTimeout = 5 * time.Second
	DefaultWSIdleTimeout       = 60 * time.Second
	DefaultWSMaxHeaderBytes    = 64 << 10
)

// WSAcceptor struct
type WSAcceptor struct {
	addr      string
	connChan  chan PlayerConn
	lifecycle acceptorLifecycle

	configMu    sync.RWMutex
	certFile    string
	keyFile     string
	checkOrigin func(*http.Request) bool

	serverMu sync.Mutex
	server   *http.Server
}

// NewWSAcceptor returns a new instance of WSAcceptor
func NewWSAcceptor(addr string, certs ...string) *WSAcceptor {
	keyFile := ""
	certFile := ""
	if len(certs) != 2 && len(certs) != 0 {
		panic(constants.ErrInvalidCertificates)
	} else if len(certs) == 2 {
		certFile = certs[0]
		keyFile = certs[1]
	}

	w := &WSAcceptor{
		addr:      addr,
		connChan:  make(chan PlayerConn),
		lifecycle: newAcceptorLifecycle(),
		certFile:  certFile,
		keyFile:   keyFile,
	}
	return w
}

func (w *WSAcceptor) IsRunning() bool {
	return w.lifecycle.isRunning()
}

func (w *WSAcceptor) GetConfiguredAddress() string {
	return w.addr
}

// GetAddr returns the addr the acceptor will listen on
func (w *WSAcceptor) GetAddr() string {
	return w.lifecycle.boundAddr()
}

// GetConnChan gets a connection channel
func (w *WSAcceptor) GetConnChan() chan PlayerConn {
	return w.connChan
}

// PROXY protocol support not implemented for WS acceptor
func (w *WSAcceptor) EnableProxyProtocol() {
}

// SetCheckOrigin sets the WebSocket Origin policy used for subsequent Serve.
// A nil policy restores the default same-origin check.
func (w *WSAcceptor) SetCheckOrigin(checkOrigin func(*http.Request) bool) {
	w.configMu.Lock()
	defer w.configMu.Unlock()
	w.checkOrigin = checkOrigin
}

type connHandler struct {
	upgrader *websocket.Upgrader
	connChan chan PlayerConn
	done     <-chan struct{}
}

func (h *connHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		logger.Zap.Error("Upgrade failure", zap.String("URI", r.RequestURI), zap.Error(err))
		return
	}
	conn.SetReadLimit(codec.HeadLength + codec.MaxPacketSize)

	c, err := NewWSConn(conn, r)
	if err != nil {
		_ = conn.Close()
		logger.Zap.Error("Failed to create new ws connection", zap.Error(err))
		return
	}
	select {
	case h.connChan <- c:
	case <-r.Context().Done():
		_ = c.Close()
	case <-h.done:
		_ = c.Close()
	}
}

// Ready returns an independent channel which receives and then closes with the
// one-time bind result. Calls made after startup replay the same result.
func (w *WSAcceptor) Ready() <-chan error {
	return w.lifecycle.readyChan()
}

// ListenAndServe is a compatibility wrapper around Serve.
func (w *WSAcceptor) ListenAndServe() error {
	return w.Serve(context.Background())
}

// ListenAndServeTLS listens and serve in the specified addr using tls
func (w *WSAcceptor) ListenAndServeTLS(cert, key string) error {
	if err := w.lifecycle.begin(); err != nil {
		return err
	}

	w.configMu.Lock()
	w.certFile = cert
	w.keyFile = key
	w.configMu.Unlock()
	return w.serveStarted(context.Background())
}

// Serve binds the configured address and serves WebSocket upgrades until ctx
// is canceled, Stop is called, or the HTTP server returns an error.
func (w *WSAcceptor) Serve(ctx context.Context) error {
	if err := w.lifecycle.begin(); err != nil {
		return err
	}
	return w.serveStarted(ctx)
}

func (w *WSAcceptor) serveStarted(ctx context.Context) error {
	if ctx == nil {
		w.lifecycle.fail(ErrNilServeContext)
		return ErrNilServeContext
	}

	certFile, keyFile, checkOrigin := w.configSnapshot()
	var tlsConfig *tls.Config
	if certFile != "" || keyFile != "" {
		crt, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			err = fmt.Errorf("%w: %v", constants.ErrInvalidCertificates, err)
			w.lifecycle.fail(err)
			return err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{crt}}
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", w.addr)
	if err != nil {
		w.lifecycle.fail(err)
		return err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}

	server := &http.Server{
		Handler: &connHandler{
			upgrader: newWSUpgrader(checkOrigin),
			connChan: w.connChan,
			done:     w.lifecycle.doneChan(),
		},
		ReadHeaderTimeout: DefaultWSReadHeaderTimeout,
		IdleTimeout:       DefaultWSIdleTimeout,
		MaxHeaderBytes:    DefaultWSMaxHeaderBytes,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	w.serverMu.Lock()
	w.server = server
	w.serverMu.Unlock()

	if err := w.lifecycle.publish(listener); err != nil {
		return err
	}

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			w.Stop()
		case <-watchDone:
		}
	}()

	err = server.Serve(listener)
	close(watchDone)
	stopped := ctx.Err() != nil || !w.IsRunning()
	w.Stop()
	if stopped || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("serve WebSocket connections: %w", err)
}

func (w *WSAcceptor) configSnapshot() (string, string, func(*http.Request) bool) {
	w.configMu.RLock()
	defer w.configMu.RUnlock()
	return w.certFile, w.keyFile, w.checkOrigin
}

func newWSUpgrader(checkOrigin func(*http.Request) bool) *websocket.Upgrader {
	if checkOrigin == nil {
		checkOrigin = checkSameOrigin
	}
	return &websocket.Upgrader{
		ReadBufferSize:  constants.IOBufferBytesSize,
		WriteBufferSize: constants.IOBufferBytesSize,
		CheckOrigin:     checkOrigin,
	}
}

// checkSameOrigin accepts non-browser clients without Origin and otherwise
// requires both the Origin host and scheme to match the upgrade transport.
func checkSameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.Host == "" || !strings.EqualFold(origin.Host, r.Host) {
		return false
	}
	expectedScheme := "http"
	if r.TLS != nil {
		expectedScheme = "https"
	}
	return strings.EqualFold(origin.Scheme, expectedScheme)
}

// Stop stops the acceptor
func (w *WSAcceptor) Stop() {
	w.lifecycle.stop()

	w.serverMu.Lock()
	server := w.server
	w.serverMu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

// WSConn is an adapter to t.Conn, which implements all t.Conn
// interface base on *websocket.Conn
type WSConn struct {
	conn   *websocket.Conn
	typ    int // message type
	reader io.Reader
	req    *http.Request
}

// NewWSConn return an initialized *WSConn
func NewWSConn(conn *websocket.Conn, r ...*http.Request) (*WSConn, error) {
	if conn != nil {
		conn.SetReadLimit(codec.HeadLength + codec.MaxPacketSize)
	}
	c := &WSConn{conn: conn}
	if len(r) > 0 {
		c.req = r[0]
	}
	return c, nil
}

func (c *WSConn) InnerConn() *websocket.Conn {
	return c.conn
}

func (c *WSConn) InnerReq() *http.Request {
	return c.req
}

// GetNextMessage reads the next message available in the stream
func (c *WSConn) GetNextMessage() (b []byte, err error) {
	_, reader, err := c.conn.NextReader()
	if err != nil {
		return nil, err
	}

	header := make([]byte, codec.HeadLength)
	if _, err := io.ReadFull(reader, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, packet.ErrInvalidPomeloHeader
		}
		return nil, err
	}
	msgSize, _, err := codec.ParseHeader(header)
	if err != nil {
		return nil, err
	}

	// Allocate only after the declared packet size has passed codec validation.
	msgBytes := make([]byte, codec.HeadLength+msgSize)
	copy(msgBytes, header)
	if _, err := io.ReadFull(reader, msgBytes[codec.HeadLength:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, constants.ErrReceivedMsgSmallerThanExpected
		}
		return nil, err
	}

	var extra [1]byte
	n, err := reader.Read(extra[:])
	if n > 0 {
		return nil, constants.ErrReceivedMsgBiggerThanExpected
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err == nil {
		// websocket message readers normally return EOF at the message boundary;
		// handle a legal zero-length read without treating it as completion.
		n, err = io.ReadFull(reader, extra[:])
		if n > 0 {
			return nil, constants.ErrReceivedMsgBiggerThanExpected
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	return msgBytes, nil
}

// Read reads data from the connection.
// Read can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetReadDeadline.
func (c *WSConn) Read(b []byte) (int, error) {
	if c.reader == nil {
		t, r, err := c.conn.NextReader()
		if err != nil {
			return 0, err
		}
		c.typ = t
		c.reader = r
	}
	n, err := c.reader.Read(b)
	if err != nil && err != io.EOF {
		return n, err
	} else if err == io.EOF {
		_, r, err := c.conn.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}

	return n, nil
}

// Write writes data to the connection.
// Write can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
func (c *WSConn) Write(b []byte) (int, error) {
	err := c.conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (c *WSConn) Close() error {
	return c.conn.Close()
}

// LocalAddr returns the local network address.
func (c *WSConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *WSConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail with a timeout (see type Error) instead of
// blocking. The deadline applies to all future and pending
// I/O, not just the immediately following call to Read or
// Write. After a deadline has been exceeded, the connection
// can be refreshed by setting a deadline in the future.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful Read or Write calls.
//
// A zero value for t means I/O operations will not time out.
func (c *WSConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}

	return c.SetWriteDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
func (c *WSConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (c *WSConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}
