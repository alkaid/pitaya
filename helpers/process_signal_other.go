//go:build !unix && !windows

package helpers

import "os"

type processControl struct {
	process *os.Process
}

func newProcessControl(process *os.Process) (*processControl, error) {
	return &processControl{process: process}, nil
}

func (c *processControl) terminate() (bool, error) {
	return true, c.process.Kill()
}

func (c *processControl) kill() error {
	return c.process.Kill()
}

func (c *processControl) release() error {
	return nil
}

func isExpectedProcessExit(_ *os.ProcessState, hardKill bool) bool {
	return hardKill
}
