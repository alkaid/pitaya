//go:build unix

package helpers

import (
	"errors"
	"os"
	"syscall"
)

type processControl struct {
	process *os.Process
}

func newProcessControl(process *os.Process) (*processControl, error) {
	return &processControl{process: process}, nil
}

func (c *processControl) terminate() (bool, error) {
	return false, c.signal(syscall.SIGTERM)
}

func (c *processControl) kill() error {
	return c.signal(syscall.SIGKILL)
}

func (c *processControl) release() error {
	err := c.kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (c *processControl) signal(signal syscall.Signal) error {
	err := syscall.Kill(-c.process.Pid, signal)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

func isExpectedProcessExit(state *os.ProcessState, hardKill bool) bool {
	if state == nil {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	return status.Signal() == syscall.SIGTERM || hardKill && status.Signal() == syscall.SIGKILL
}
