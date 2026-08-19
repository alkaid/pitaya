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
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/constants"
)

// TCPAcceptor struct
type TCPAcceptor struct {
	addr      string
	connChan  chan PlayerConn
	lifecycle acceptorLifecycle

	configMu                   sync.RWMutex
	certs                      []tls.Certificate
	proxyProtocol              bool
	proxyProtocolHeaderTimeout time.Duration
}

type tcpPlayerConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (t *tcpPlayerConn) RemoteAddr() net.Addr {
	return t.remoteAddr
}

// GetNextMessage reads the next message available in the stream
func (t *tcpPlayerConn) GetNextMessage() (b []byte, err error) {
	header, err := io.ReadAll(io.LimitReader(t.Conn, codec.HeadLength))
	if err != nil {
		return nil, err
	}
	// if the header has no data, we can consider it as a closed connection
	if len(header) == 0 {
		return nil, constants.ErrConnectionClosed
	}
	msgSize, _, err := codec.ParseHeader(header)
	if err != nil {
		return nil, err
	}
	msgData, err := io.ReadAll(io.LimitReader(t.Conn, int64(msgSize)))
	if err != nil {
		return nil, err
	}
	if len(msgData) < msgSize {
		return nil, constants.ErrReceivedMsgSmallerThanExpected
	}
	return append(header, msgData...), nil
}

// NewTCPAcceptor creates a new instance of tcp acceptor
func NewTCPAcceptor(addr string, certs ...string) *TCPAcceptor {
	certificates := []tls.Certificate{}
	if len(certs) != 2 && len(certs) != 0 {
		panic(constants.ErrIncorrectNumberOfCertificates)
	} else if len(certs) == 2 && certs[0] != "" && certs[1] != "" {
		cert, err := tls.LoadX509KeyPair(certs[0], certs[1])
		if err != nil {
			panic(fmt.Errorf("%w: %v", constants.ErrInvalidCertificates, err))
		}
		certificates = append(certificates, cert)
	}

	return NewTLSAcceptor(addr, certificates...)
}

func NewTLSAcceptor(addr string, certs ...tls.Certificate) *TCPAcceptor {
	return &TCPAcceptor{
		addr:                       addr,
		connChan:                   make(chan PlayerConn),
		lifecycle:                  newAcceptorLifecycle(),
		certs:                      certs,
		proxyProtocolHeaderTimeout: DefaultProxyProtocolHeaderTimeout,
	}
}

// GetAddr returns the addr the acceptor will listen on
func (a *TCPAcceptor) GetAddr() string {
	return a.lifecycle.boundAddr()
}

// GetConnChan gets a connection channel
func (a *TCPAcceptor) GetConnChan() chan PlayerConn {
	return a.connChan
}

// Stop stops the acceptor
func (a *TCPAcceptor) Stop() {
	a.lifecycle.stop()
}

// Ready returns an independent channel which receives and then closes with the
// one-time bind result. Calls made after startup replay the same result.
func (a *TCPAcceptor) Ready() <-chan error {
	return a.lifecycle.readyChan()
}

// ListenAndServe is a compatibility wrapper around Serve.
func (a *TCPAcceptor) ListenAndServe() error {
	return a.Serve(context.Background())
}

// ListenAndServeTLS listens using tls
func (a *TCPAcceptor) ListenAndServeTLS(cert, key string) error {
	if err := a.lifecycle.begin(); err != nil {
		return err
	}

	crt, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		err = fmt.Errorf("%w: %v", constants.ErrInvalidCertificates, err)
		a.lifecycle.fail(err)
		return err
	}

	a.configMu.Lock()
	a.certs = append(a.certs, crt)
	a.configMu.Unlock()
	return a.serveStarted(context.Background())
}

// Serve binds the configured address and accepts connections until ctx is
// canceled, Stop is called, or the listener returns an error.
func (a *TCPAcceptor) Serve(ctx context.Context) error {
	if err := a.lifecycle.begin(); err != nil {
		return err
	}
	return a.serveStarted(ctx)
}

func (a *TCPAcceptor) serveStarted(ctx context.Context) error {
	if ctx == nil {
		a.lifecycle.fail(ErrNilServeContext)
		return ErrNilServeContext
	}

	certs, proxyProtocol, proxyHeaderTimeout := a.configSnapshot()
	listener, err := a.createBaseListener(ctx, proxyProtocol, proxyHeaderTimeout)
	if err != nil {
		a.lifecycle.fail(err)
		return err
	}
	if len(certs) > 0 {
		listener = a.listenAndServeTLS(listener, certs)
	}
	if err := a.lifecycle.publish(listener); err != nil {
		return err
	}

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			a.Stop()
		case <-watchDone:
		}
	}()

	err = a.serve(ctx, listener)
	close(watchDone)
	a.Stop()
	return err
}

func (a *TCPAcceptor) configSnapshot() ([]tls.Certificate, bool, time.Duration) {
	a.configMu.RLock()
	defer a.configMu.RUnlock()

	certs := append([]tls.Certificate(nil), a.certs...)
	return certs, a.proxyProtocol, a.proxyProtocolHeaderTimeout
}

// Create base listener
func (a *TCPAcceptor) createBaseListener(ctx context.Context, proxyProtocol bool, proxyHeaderTimeout time.Duration) (net.Listener, error) {
	baseListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", a.addr)
	if err != nil {
		return nil, err
	}

	return &ProxyProtocolListener{
		Listener:             baseListener,
		ProxyProtocolEnabled: proxyProtocol,
		HeaderTimeout:        proxyHeaderTimeout,
	}, nil
}

// ListenAndServeTLS listens using tls
func (a *TCPAcceptor) listenAndServeTLS(listener net.Listener, certs []tls.Certificate) net.Listener {
	return tls.NewListener(listener, &tls.Config{Certificates: certs})
}

func (a *TCPAcceptor) EnableProxyProtocol() {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.proxyProtocol = true
}

func (a *TCPAcceptor) serve(ctx context.Context, listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if isProxyProtocolError(err) {
				continue
			}
			if ctx.Err() != nil || !a.IsRunning() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept TCP connection: %w", err)
		}

		playerConn := &tcpPlayerConn{
			Conn:       conn,
			remoteAddr: conn.RemoteAddr(),
		}
		select {
		case a.connChan <- playerConn:
		case <-ctx.Done():
			_ = conn.Close()
			return nil
		case <-a.lifecycle.doneChan():
			_ = conn.Close()
			return nil
		}
	}
}

func (a *TCPAcceptor) IsRunning() bool {
	return a.lifecycle.isRunning()
}

func (a *TCPAcceptor) GetConfiguredAddress() string {
	return a.addr
}
