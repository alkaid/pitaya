package helpers

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestManagedProcessHelper(t *testing.T) {
	if os.Getenv("PITAYA_MANAGED_PROCESS_HELPER") != "1" {
		return
	}

	switch os.Getenv("PITAYA_MANAGED_PROCESS_MODE") {
	case "ready":
		fmt.Fprintln(os.Stderr, "starting")
		fmt.Fprintln(os.Stdout, serverReadyMarker)
		select {}
	case "exit":
		fmt.Fprintln(os.Stderr, "startup failed")
		os.Exit(23)
	case "timeout":
		fmt.Fprintln(os.Stderr, "still starting")
		select {}
	default:
		os.Exit(24)
	}
}

func managedProcessCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedProcessHelper$")
	cmd.Env = append(os.Environ(),
		"PITAYA_MANAGED_PROCESS_HELPER=1",
		"PITAYA_MANAGED_PROCESS_MODE="+mode,
	)
	return cmd
}

func TestStartReadyProcessAndIdempotentStop(t *testing.T) {
	process, err := startReadyProcess(managedProcessCommand("ready"), serverReadyMarker, time.Second)
	if err != nil {
		t.Fatalf("startReadyProcess() error = %v", err)
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestStartReadyProcessReportsEarlyExit(t *testing.T) {
	_, err := startReadyProcess(managedProcessCommand("exit"), serverReadyMarker, time.Second)
	if err == nil {
		t.Fatal("startReadyProcess() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "startup failed") {
		t.Fatalf("startReadyProcess() error = %q, want output tail", err)
	}
}

func TestStartReadyProcessReportsTimeout(t *testing.T) {
	_, err := startReadyProcess(managedProcessCommand("timeout"), serverReadyMarker, time.Second)
	if err == nil {
		t.Fatal("startReadyProcess() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("startReadyProcess() error = %q, want output tail", err)
	}
}

func TestStartReadyProcessInParallel(t *testing.T) {
	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("process-%d", i), func(t *testing.T) {
			t.Parallel()
			process, err := startReadyProcess(managedProcessCommand("ready"), serverReadyMarker, time.Second)
			if err != nil {
				t.Fatalf("startReadyProcess() error = %v", err)
			}
			if err := process.Stop(); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
		})
	}
}

func TestProcessOutputKeepsLastLines(t *testing.T) {
	output := newProcessOutput("ready", 3)
	_, _ = output.Write([]byte("one\ntwo\nthree\nfour\n"))
	if got, want := output.tail(), "two\nthree\nfour"; got != want {
		t.Fatalf("tail() = %q, want %q", got, want)
	}
}

func TestProcessOutputBoundsPartialLineAndDetectsMarker(t *testing.T) {
	output := newProcessOutput(serverReadyMarker, 3)
	prefix := strings.Repeat("x", processOutputPendingLimit+100)
	_, _ = output.Write([]byte(prefix + serverReadyMarker[:3]))
	if got := len(output.tail()); got != processOutputPendingLimit {
		t.Fatalf("tail length = %d, want %d", got, processOutputPendingLimit)
	}

	_, _ = output.Write([]byte(serverReadyMarker[3:]))
	select {
	case <-output.ready:
	default:
		t.Fatal("readiness marker spanning bounded writes was not detected")
	}
}

func TestProcessOutputBoundsCompletedLineAndDetectsMarker(t *testing.T) {
	output := newProcessOutput(serverReadyMarker, 3)
	line := serverReadyMarker + strings.Repeat("x", processOutputPendingLimit+100)
	_, _ = output.Write([]byte(line + "\n"))

	select {
	case <-output.ready:
	default:
		t.Fatal("readiness marker in truncated completed line was not detected")
	}
	if got := len(output.tail()); got != processOutputPendingLimit {
		t.Fatalf("tail length = %d, want %d", got, processOutputPendingLimit)
	}
}

func TestManagedProcessRecordsNaturalExitAfterStopRequest(t *testing.T) {
	cmd := managedProcessCommand("exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("helper process unexpectedly exited successfully")
	}

	process := &managedProcess{cmd: cmd, waitErr: waitErr}
	process.recordWaitError(true, false)
	if process.stopErr == nil {
		t.Fatal("natural process failure was suppressed as requested shutdown")
	}
}

func TestManagedProcessSuppressesRequestedHardKill(t *testing.T) {
	cmd := managedProcessCommand("timeout")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("killed helper process unexpectedly exited successfully")
	}

	process := &managedProcess{cmd: cmd, waitErr: waitErr}
	process.recordWaitError(true, true)
	if process.stopErr != nil {
		t.Fatalf("requested hard kill returned error: %v", process.stopErr)
	}
}
