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

package agent

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingWriteConn struct {
	firstWriteStarted chan struct{}
	releaseWrites     chan struct{}
	firstWriteOnce    sync.Once
	activeWrites      atomic.Int32
	maxActiveWrites   atomic.Int32
}

func (c *blockingWriteConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *blockingWriteConn) Write(data []byte) (int, error) {
	active := c.activeWrites.Add(1)
	for {
		maxActive := c.maxActiveWrites.Load()
		if active <= maxActive || c.maxActiveWrites.CompareAndSwap(maxActive, active) {
			break
		}
	}

	c.firstWriteOnce.Do(func() { close(c.firstWriteStarted) })

	<-c.releaseWrites
	c.activeWrites.Add(-1)
	return len(data), nil
}

func (*blockingWriteConn) Close() error {
	return nil
}

func (*blockingWriteConn) LocalAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*blockingWriteConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*blockingWriteConn) SetDeadline(time.Time) error {
	return nil
}

func (*blockingWriteConn) SetReadDeadline(time.Time) error {
	return nil
}

func (*blockingWriteConn) SetWriteDeadline(time.Time) error {
	return nil
}

func TestConnWriteWithDeadlineSerializesConcurrentWriters(t *testing.T) {
	conn := &blockingWriteConn{
		firstWriteStarted: make(chan struct{}),
		releaseWrites:     make(chan struct{}),
	}
	agent := &agentImpl{conn: conn}

	firstDone := make(chan error, 1)
	go func() {
		_, err := agent.connWriteWithDeadline([]byte("first"))
		firstDone <- err
	}()

	select {
	case <-conn.firstWriteStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first write")
	}

	time.AfterFunc(10*time.Millisecond, func() { close(conn.releaseWrites) })
	if _, err := agent.connWriteWithDeadline([]byte("second")); err != nil {
		t.Fatalf("second write returned error: %v", err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first write returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first write")
	}

	if got := conn.maxActiveWrites.Load(); got != 1 {
		t.Fatalf("maximum concurrent writes = %d, want 1", got)
	}
}
