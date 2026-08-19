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
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/acceptor"

	"github.com/alkaid/pitaya/v2/mocks"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
)

const wrapperSubmissionErrorHelperEnv = "PITAYA_TEST_WRAPPER_SUBMISSION_ERROR"

func TestListenAndServe(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockAcceptor := mocks.NewMockAcceptor(ctrl)
	mockConn := mocks.NewMockPlayerConn(ctrl)

	conns := make(chan acceptor.PlayerConn)
	exit := make(chan struct{})
	reads := 3
	go func() {
		for i := 0; i < reads; i++ {
			mockConn.EXPECT().Read([]byte{})
			conns <- mockConn
		}
	}()

	mockAcceptor.EXPECT().GetConnChan().Return(conns)
	wrapper := NewBaseWrapper(func(c acceptor.PlayerConn) acceptor.PlayerConn {
		_, err := c.Read([]byte{})
		assert.NoError(t, err)
		return c
	})
	wrapper.Acceptor = mockAcceptor
	firstReady := wrapper.Ready()
	secondReady := wrapper.Ready()

	go func() {
		i := 0
		for range wrapper.GetConnChan() {
			i++
			if i == reads {
				close(exit)
			}
		}
	}()

	mockAcceptor.EXPECT().Serve(gomock.Any()).DoAndReturn(func(context.Context) error {
		<-exit
		return nil
	})
	ready := make(chan error, 1)
	ready <- nil
	mockAcceptor.EXPECT().Ready().Return(ready)
	assert.NoError(t, wrapper.Serve(context.Background()))
	assert.NoError(t, <-firstReady)
	assert.NoError(t, <-secondReady)
	assert.NoError(t, <-wrapper.Ready())
}

func TestServeNilContextReturnsErrorWithoutPanicking(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockAcceptor := mocks.NewMockAcceptor(ctrl)
	wrapper := NewBaseWrapper(func(conn acceptor.PlayerConn) acceptor.PlayerConn { return conn })
	wrapper.Acceptor = mockAcceptor

	mockAcceptor.EXPECT().Serve(nil).Return(acceptor.ErrNilServeContext)
	requireErr := wrapper.Serve(nil)
	assert.ErrorIs(t, requireErr, acceptor.ErrNilServeContext)
	assert.ErrorIs(t, <-wrapper.Ready(), acceptor.ErrNilServeContext)
}

func TestPipeClosesWrappedConnectionWhenCanceledBeforeDelivery(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockAcceptor := mocks.NewMockAcceptor(ctrl)
	mockConn := mocks.NewMockPlayerConn(ctrl)
	conns := make(chan acceptor.PlayerConn, 1)
	conns <- mockConn
	wrapped := make(chan struct{})

	wrapper := NewBaseWrapper(func(conn acceptor.PlayerConn) acceptor.PlayerConn {
		close(wrapped)
		return conn
	})
	wrapper.Acceptor = mockAcceptor
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	ready <- nil

	mockAcceptor.EXPECT().GetConnChan().Return(conns)
	mockAcceptor.EXPECT().Ready().Return(ready)
	mockAcceptor.EXPECT().Serve(ctx).DoAndReturn(func(context.Context) error {
		<-ctx.Done()
		return nil
	})
	mockConn.EXPECT().Close().Return(nil)

	done := make(chan error, 1)
	go func() { done <- wrapper.Serve(ctx) }()
	assert.NoError(t, <-wrapper.Ready())
	<-wrapped
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("wrapper Serve did not return after cancellation")
	}
}

func TestPipeSubmissionFailureResolvesReady(t *testing.T) {
	if os.Getenv(wrapperSubmissionErrorHelperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPipeSubmissionFailureResolvesReady$")
		cmd.Env = append(os.Environ(), wrapperSubmissionErrorHelperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("wrapper submission helper failed: %v\n%s", err, output)
		}
		return
	}

	underlying := acceptor.NewTCPAcceptor("127.0.0.1:0")
	wrapper := NewBaseWrapper(func(conn acceptor.PlayerConn) acceptor.PlayerConn { return conn })
	wrapper.Acceptor = underlying
	ants.Release()

	err := wrapper.Serve(context.Background())
	if !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("Serve error = %v, want %v", err, ants.ErrPoolClosed)
	}
	if readyErr := <-wrapper.Ready(); !errors.Is(readyErr, ants.ErrPoolClosed) {
		t.Fatalf("Ready error = %v, want %v", readyErr, ants.ErrPoolClosed)
	}
}

type countingLifecycleAcceptor struct {
	connChan     chan acceptor.PlayerConn
	ready        chan error
	pipeStarted  chan struct{}
	serveStarted chan struct{}
	serveCalls   atomic.Int32
	readyCalls   atomic.Int32
	connCalls    atomic.Int32
}

func newCountingLifecycleAcceptor() *countingLifecycleAcceptor {
	return &countingLifecycleAcceptor{
		connChan:     make(chan acceptor.PlayerConn),
		ready:        make(chan error, 1),
		pipeStarted:  make(chan struct{}),
		serveStarted: make(chan struct{}),
	}
}

func (a *countingLifecycleAcceptor) Serve(ctx context.Context) error {
	if a.serveCalls.Add(1) != 1 {
		return acceptor.ErrAcceptorAlreadyStarted
	}
	close(a.serveStarted)
	a.ready <- nil
	<-ctx.Done()
	return nil
}

func (a *countingLifecycleAcceptor) Ready() <-chan error {
	a.readyCalls.Add(1)
	return a.ready
}

func (a *countingLifecycleAcceptor) Stop() {}

func (a *countingLifecycleAcceptor) GetAddr() string { return "" }

func (a *countingLifecycleAcceptor) GetConnChan() chan acceptor.PlayerConn {
	if a.connCalls.Add(1) == 1 {
		close(a.pipeStarted)
	}
	return a.connChan
}

func (a *countingLifecycleAcceptor) EnableProxyProtocol() {}

func (a *countingLifecycleAcceptor) IsRunning() bool { return true }

func (a *countingLifecycleAcceptor) GetConfiguredAddress() string { return "" }

func TestConcurrentRepeatedServeHasNoUnderlyingSideEffects(t *testing.T) {
	underlying := newCountingLifecycleAcceptor()
	wrapper := NewBaseWrapper(func(conn acceptor.PlayerConn) acceptor.PlayerConn { return conn })
	wrapper.Acceptor = underlying
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstDone := make(chan error, 1)
	go func() { firstDone <- wrapper.Serve(ctx) }()

	select {
	case <-underlying.pipeStarted:
	case <-time.After(time.Second):
		t.Fatal("first wrapper pipe did not start")
	}
	select {
	case <-underlying.serveStarted:
	case <-time.After(time.Second):
		t.Fatal("first underlying Serve did not start")
	}
	assert.NoError(t, <-wrapper.Ready())

	assert.ErrorIs(t, wrapper.Serve(ctx), acceptor.ErrAcceptorAlreadyStarted)
	assert.Equal(t, int32(1), underlying.serveCalls.Load())
	assert.Equal(t, int32(1), underlying.readyCalls.Load())
	assert.Equal(t, int32(1), underlying.connCalls.Load())

	cancel()
	select {
	case err := <-firstDone:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("first wrapper Serve did not stop")
	}
}

func TestStopDoesNotOverwritePublishedUnderlyingReady(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockAcceptor := mocks.NewMockAcceptor(ctrl)
	wrapper := NewBaseWrapper(func(conn acceptor.PlayerConn) acceptor.PlayerConn { return conn })
	wrapper.Acceptor = mockAcceptor

	connections := make(chan acceptor.PlayerConn)
	underlyingReady := make(chan error, 1)
	underlyingReady <- nil
	readyCalled := make(chan struct{})
	releaseReady := make(chan struct{})

	mockAcceptor.EXPECT().GetConnChan().Return(connections)
	mockAcceptor.EXPECT().Ready().DoAndReturn(func() <-chan error {
		close(readyCalled)
		<-releaseReady
		return underlyingReady
	})
	mockAcceptor.EXPECT().Stop()
	mockAcceptor.EXPECT().Serve(gomock.Any()).Return(nil)

	done := make(chan error, 1)
	go func() { done <- wrapper.Serve(context.Background()) }()

	select {
	case <-readyCalled:
	case <-time.After(time.Second):
		t.Fatal("wrapper did not request underlying readiness")
	}
	wrapper.Stop()
	close(releaseReady)

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("wrapper Serve did not return")
	}
	assert.NoError(t, <-wrapper.Ready())
}
