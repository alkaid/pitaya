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

package acceptor

import (
	"context"
	"errors"
	"net"
	"sync"
)

var (
	// ErrAcceptorStopped is returned when Serve is called after Stop won the
	// startup race. Acceptors are single-use and cannot be restarted.
	ErrAcceptorStopped = errors.New("acceptor stopped")
	// ErrAcceptorAlreadyStarted is returned when Serve is called more than once.
	ErrAcceptorAlreadyStarted = errors.New("acceptor already started")
	// ErrNilServeContext is returned instead of panicking when Serve receives a
	// nil context.
	ErrNilServeContext = errors.New("acceptor serve context is nil")
)

// PlayerConn iface
type PlayerConn interface {
	GetNextMessage() (b []byte, err error)
	RemoteAddr() net.Addr
	net.Conn
}

// Acceptor type interface
type Acceptor interface {
	Serve(context.Context) error
	Ready() <-chan error
	Stop()
	GetAddr() string
	GetConnChan() chan PlayerConn
	EnableProxyProtocol()
	IsRunning() bool
	GetConfiguredAddress() string
}

type lifecycleState uint8

const (
	lifecycleNew lifecycleState = iota
	lifecycleStarting
	lifecycleRunning
	lifecycleStopped
)

// acceptorLifecycle owns the fields shared by Serve, Ready, Stop and the
// status accessors. An acceptor has one lifecycle and cannot be restarted.
type acceptorLifecycle struct {
	mu       sync.RWMutex
	state    lifecycleState
	running  bool
	listener net.Listener
	addr     string

	readyPublished bool
	readyErr       error
	readyWaiters   []chan error
	done           chan struct{}
	stopOnce       sync.Once
}

func newAcceptorLifecycle() acceptorLifecycle {
	return acceptorLifecycle{
		state: lifecycleNew,
		done:  make(chan struct{}),
	}
}

func (l *acceptorLifecycle) begin() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case lifecycleNew:
		l.state = lifecycleStarting
		return nil
	case lifecycleStopped:
		return ErrAcceptorStopped
	default:
		return ErrAcceptorAlreadyStarted
	}
}

func (l *acceptorLifecycle) publish(listener net.Listener) error {
	l.mu.Lock()
	if l.state == lifecycleStopped {
		l.mu.Unlock()
		_ = listener.Close()
		return ErrAcceptorStopped
	}
	if l.state != lifecycleStarting {
		l.mu.Unlock()
		_ = listener.Close()
		return ErrAcceptorAlreadyStarted
	}

	l.listener = listener
	l.addr = listener.Addr().String()
	l.running = true
	l.state = lifecycleRunning
	l.signalReadyLocked(nil)
	l.mu.Unlock()
	return nil
}

func (l *acceptorLifecycle) fail(err error) {
	l.signalReady(err)
	l.stop()
}

func (l *acceptorLifecycle) signalReady(err error) {
	l.mu.Lock()
	l.signalReadyLocked(err)
	l.mu.Unlock()
}

func (l *acceptorLifecycle) signalReadyLocked(err error) {
	if l.readyPublished {
		return
	}
	l.readyPublished = true
	l.readyErr = err
	for _, waiter := range l.readyWaiters {
		waiter <- err
		close(waiter)
	}
	l.readyWaiters = nil
}

func (l *acceptorLifecycle) stop() {
	l.stopOnce.Do(func() {
		l.mu.Lock()
		l.state = lifecycleStopped
		l.running = false
		listener := l.listener
		l.signalReadyLocked(ErrAcceptorStopped)
		close(l.done)
		l.mu.Unlock()

		if listener != nil {
			_ = listener.Close()
		}
	})
}

func (l *acceptorLifecycle) readyChan() <-chan error {
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

func (l *acceptorLifecycle) doneChan() <-chan struct{} {
	return l.done
}

func (l *acceptorLifecycle) isRunning() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.running
}

func (l *acceptorLifecycle) boundAddr() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.addr
}
