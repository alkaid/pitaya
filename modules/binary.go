//go:build linux || darwin

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

package modules

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
)

// Binary is a pitaya module that starts a binary as a child process and
// pipes its stdout
type Binary struct {
	Base
	binPath                  string
	args                     []string
	gracefulShutdownInterval time.Duration
	cmd                      *exec.Cmd
	exitCh                   chan struct{}
}

// NewBinary creates a new binary module with the given path
func NewBinary(binPath string, args []string, gracefulShutdownInterval ...time.Duration) *Binary {
	gracefulTime := 15 * time.Second
	if len(gracefulShutdownInterval) > 0 {
		gracefulTime = gracefulShutdownInterval[0]
	}
	return &Binary{
		binPath:                  binPath,
		args:                     args,
		gracefulShutdownInterval: gracefulTime,
		exitCh:                   make(chan struct{}),
	}
}

// GetExitChannel gets a channel that is closed when the binary dies
func (b *Binary) GetExitChannel() chan struct{} {
	return b.exitCh
}

func closeBinaryPipe(parent io.Closer, child io.Writer) {
	if parent != nil {
		_ = parent.Close()
	}
	if childCloser, ok := child.(io.Closer); ok {
		_ = childCloser.Close()
	}
}

// Init initializes the binary
func (b *Binary) Init() error {
	b.cmd = exec.Command(b.binPath, b.args...)
	stdout, err := b.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create binary stdout pipe: %w", err)
	}
	stdOutScanner := bufio.NewScanner(stdout)
	stderr, err := b.cmd.StderrPipe()
	if err != nil {
		closeBinaryPipe(stdout, b.cmd.Stdout)
		return fmt.Errorf("create binary stderr pipe: %w", err)
	}
	stdErrScanner := bufio.NewScanner(stderr)
	processStarted := make(chan struct{})
	monitors := []func(){
		func() {
			for stdOutScanner.Scan() {
				logger.Log.Info(stdOutScanner.Text())
			}
		},
		func() {
			for stdErrScanner.Scan() {
				logger.Log.Error(stdErrScanner.Text())
			}
		},
		func() {
			<-processStarted
			_ = b.cmd.Wait()
			close(b.exitCh)
		},
	}
	for _, monitor := range monitors {
		if err := co.Submit(monitor); err != nil {
			closeBinaryPipe(stdout, b.cmd.Stdout)
			closeBinaryPipe(stderr, b.cmd.Stderr)
			close(processStarted)
			return fmt.Errorf("start binary monitor: %w", err)
		}
	}

	err = b.cmd.Start()
	if err != nil {
		closeBinaryPipe(stdout, nil)
		closeBinaryPipe(stderr, nil)
		close(processStarted)
		return err
	}
	close(processStarted)
	return nil
}

// Shutdown shutdowns the binary module
func (b *Binary) Shutdown() error {
	if b.cmd == nil || b.cmd.Process == nil {
		return nil
	}
	err := b.cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		return err
	}
	timeout := time.After(b.gracefulShutdownInterval)
	select {
	case <-b.exitCh:
		return nil
	case <-timeout:
		b.cmd.Process.Kill()
		return constants.ErrTimeoutTerminatingBinaryModule
	}
}
