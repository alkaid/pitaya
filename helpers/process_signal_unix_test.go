//go:build unix

package helpers

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const managedProcessTreeMode = "PITAYA_MANAGED_PROCESS_TREE_MODE"

func TestManagedProcessTreeHelper(t *testing.T) {
	switch os.Getenv(managedProcessTreeMode) {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagedProcessTreeHelper$")
		cmd.Env = append(os.Environ(), managedProcessTreeMode+"=child")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stdout, "child-pid=%d\n", cmd.Process.Pid)
		fmt.Fprintln(os.Stdout, serverReadyMarker)
		select {}
	case "child":
		terminated := make(chan os.Signal, 1)
		signal.Notify(terminated, syscall.SIGTERM)
		<-terminated
		os.Exit(0)
	}
}

func TestManagedProcessStopsDescendantTree(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedProcessTreeHelper$")
	cmd.Env = append(os.Environ(), managedProcessTreeMode+"=parent")
	process, err := startReadyProcess(cmd, serverReadyMarker, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	childPID := childPIDFromOutput(t, process.output.tail())
	defer syscall.Kill(childPID, syscall.SIGKILL)
	if err := process.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		err := syscall.Kill(childPID, 0)
		if err == syscall.ESRCH {
			break
		}
		if err != nil {
			t.Fatalf("probe child process %d: %v", childPID, err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("descendant process %d survived Stop()", childPID)
		case <-poll.C:
		}
	}
}

func childPIDFromOutput(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		value, found := strings.CutPrefix(line, "child-pid=")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("parse child PID %q: %v", value, err)
		}
		return pid
	}
	t.Fatalf("child PID missing from output:\n%s", output)
	return 0
}

func TestManagedProcessRecordsUnexpectedSignalAfterStopRequest(t *testing.T) {
	cmd := managedProcessCommand("timeout")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("signaled helper process unexpectedly exited successfully")
	}

	process := &managedProcess{cmd: cmd, waitErr: waitErr}
	process.recordWaitError(true, true)
	if process.stopErr == nil {
		t.Fatal("unexpected signal was suppressed as requested shutdown")
	}
}
