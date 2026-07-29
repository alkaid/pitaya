//go:build windows

package helpers

import (
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processControl struct {
	job       windows.Handle
	closeOnce sync.Once
	closeErr  error
}

func newProcessControl(process *os.Process) (*processControl, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}

	assigned := false
	defer func() {
		if !assigned {
			_ = windows.CloseHandle(job)
		}
	}()

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return nil, err
	}

	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(process.Pid),
	)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(processHandle)

	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		return nil, err
	}
	assigned = true
	return &processControl{job: job}, nil
}

func (c *processControl) terminate() (bool, error) {
	return true, c.closeJob()
}

func (c *processControl) kill() error {
	return c.closeJob()
}

func (c *processControl) release() error {
	return c.closeJob()
}

func (c *processControl) closeJob() error {
	c.closeOnce.Do(func() {
		c.closeErr = windows.CloseHandle(c.job)
	})
	return c.closeErr
}

func isExpectedProcessExit(_ *os.ProcessState, hardKill bool) bool {
	return hardKill
}
