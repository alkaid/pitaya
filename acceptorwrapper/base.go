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

package acceptorwrapper

import (
	"context"
	"sync"

	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/co"
)

// BaseWrapper implements Wrapper by saving the acceptor as an attribute.
// Conns from acceptor.GetConnChan are processed by wrapConn and
// forwarded to its own connChan.
// Any new wrapper can inherit from BaseWrapper and just implement wrapConn.
type BaseWrapper struct {
	acceptor.Acceptor
	connChan  chan acceptor.PlayerConn
	wrapConn  func(acceptor.PlayerConn) acceptor.PlayerConn
	lifecycle *baseWrapperLifecycle
}

type baseWrapperLifecycleState uint8

const (
	baseWrapperLifecycleNew baseWrapperLifecycleState = iota
	baseWrapperLifecycleServing
	baseWrapperLifecycleStopped
)

type baseWrapperLifecycle struct {
	mu             sync.Mutex
	state          baseWrapperLifecycleState
	readyPublished bool
	readyErr       error
	readyWaiters   []chan error
}

// NewBaseWrapper returns an instance of BaseWrapper.
func NewBaseWrapper(wrapConn func(acceptor.PlayerConn) acceptor.PlayerConn) BaseWrapper {
	return BaseWrapper{
		connChan:  make(chan acceptor.PlayerConn),
		wrapConn:  wrapConn,
		lifecycle: &baseWrapperLifecycle{},
	}
}

// ListenAndServe preserves the legacy concrete wrapper API.
func (b *BaseWrapper) ListenAndServe() {
	_ = b.Serve(context.Background())
}

// Serve forwards the wrapped acceptor lifecycle while keeping the connection
// pipe in the same cancellation scope.
func (b *BaseWrapper) Serve(ctx context.Context) error {
	if err := b.lifecycle.begin(); err != nil {
		return err
	}
	defer b.lifecycle.finish()

	if ctx == nil {
		err := b.Acceptor.Serve(nil)
		b.signalReady(err)
		return err
	}

	pipeCtx, cancelPipe := context.WithCancel(ctx)
	defer cancelPipe()

	pipeDone := make(chan struct{})
	if err := co.Submit(func() {
		defer close(pipeDone)
		b.pipe(pipeCtx)
	}); err != nil {
		b.signalReady(err)
		b.Acceptor.Stop()
		return err
	}

	serveDone := make(chan error, 1)
	underlyingReady := b.Acceptor.Ready()
	go func() {
		serveDone <- b.Acceptor.Serve(ctx)
	}()

	var err error
	select {
	case readyErr, ok := <-underlyingReady:
		if !ok {
			readyErr = acceptor.ErrAcceptorStopped
		}
		b.signalReady(readyErr)
		err = <-serveDone
	case err = <-serveDone:
		select {
		case readyErr, ok := <-underlyingReady:
			if !ok {
				readyErr = acceptor.ErrAcceptorStopped
			}
			b.signalReady(readyErr)
		default:
			readyErr := err
			if readyErr == nil {
				readyErr = acceptor.ErrAcceptorStopped
			}
			b.signalReady(readyErr)
		}
	}

	cancelPipe()
	<-pipeDone
	return err
}

// Ready includes wrapper-local startup failures as well as the wrapped
// acceptor's bind result.
func (b *BaseWrapper) Ready() <-chan error {
	return b.lifecycle.readyChan()
}

// Stop keeps the wrapper's readiness contract valid even before Serve starts.
func (b *BaseWrapper) Stop() {
	if b.lifecycle.stop() {
		b.signalReady(acceptor.ErrAcceptorStopped)
	}
	b.Acceptor.Stop()
}

func (l *baseWrapperLifecycle) begin() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case baseWrapperLifecycleNew:
		l.state = baseWrapperLifecycleServing
		return nil
	case baseWrapperLifecycleStopped:
		return acceptor.ErrAcceptorStopped
	default:
		return acceptor.ErrAcceptorAlreadyStarted
	}
}

func (l *baseWrapperLifecycle) finish() {
	l.mu.Lock()
	l.state = baseWrapperLifecycleStopped
	l.mu.Unlock()
}

// stop reports whether Stop won before Serve claimed the one-time lifecycle.
func (l *baseWrapperLifecycle) stop() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	wasNew := l.state == baseWrapperLifecycleNew
	l.state = baseWrapperLifecycleStopped
	return wasNew
}

func (b *BaseWrapper) signalReady(err error) {
	b.lifecycle.signalReady(err)
}

func (l *baseWrapperLifecycle) readyChan() <-chan error {
	ready := make(chan error, 1)
	l.mu.Lock()
	if l.readyPublished {
		ready <- l.readyErr
		close(ready)
	} else {
		l.readyWaiters = append(l.readyWaiters, ready)
	}
	l.mu.Unlock()
	return ready
}

func (l *baseWrapperLifecycle) signalReady(err error) {
	l.mu.Lock()
	if l.readyPublished {
		l.mu.Unlock()
		return
	}
	l.readyPublished = true
	l.readyErr = err
	for _, waiter := range l.readyWaiters {
		waiter <- err
		close(waiter)
	}
	l.readyWaiters = nil
	l.mu.Unlock()
}

// GetConnChan returns the wrapper conn chan
func (b *BaseWrapper) GetConnChan() chan acceptor.PlayerConn {
	return b.connChan
}

func (b *BaseWrapper) pipe(ctx context.Context) {
	source := b.Acceptor.GetConnChan()
	for {
		select {
		case conn, ok := <-source:
			if !ok {
				return
			}
			wrapped := b.wrapConn(conn)
			select {
			case b.connChan <- wrapped:
			case <-ctx.Done():
				if wrapped != nil {
					_ = wrapped.Close()
				}
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
