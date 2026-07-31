package helpers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	case "clean-exit":
		fmt.Fprintln(os.Stderr, "startup stopped")
		return
	case "timeout":
		fmt.Fprintln(os.Stderr, "still starting")
		select {}
	case "cancel":
		if err := os.WriteFile(os.Getenv("PITAYA_MANAGED_PROCESS_STARTED_FILE"), []byte("started"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(25)
		}
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

func TestTestServerProcessDelegatesLifecycleAndDiagnostics(t *testing.T) {
	managed, err := startReadyProcess(managedProcessCommand("ready"), serverReadyMarker, time.Second)
	if err != nil {
		t.Fatalf("startReadyProcess() error = %v", err)
	}
	process := &TestServerProcess{process: managed, port: 43210, readyDir: t.TempDir()}
	if got, want := process.Port(), 43210; got != want {
		t.Fatalf("Port() = %d, want %d", got, want)
	}
	if !strings.Contains(process.Tail(), serverReadyMarker) {
		t.Fatalf("Tail() = %q, want readiness marker", process.Tail())
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := process.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	select {
	case <-process.Done():
	default:
		t.Fatal("Done() was not closed after Stop()")
	}
	if err := process.Wait(); err == nil {
		t.Fatal("Wait() unexpectedly hid the raw terminated-process error")
	}
}

func TestTestServerEnvironmentCanRemoveInheritedPitayaConfig(t *testing.T) {
	t.Setenv("PITAYA_BENCHMARK_UNSAFE_OVERRIDE", "enabled")
	for _, value := range testServerEnvironment(true) {
		if strings.HasPrefix(value, "PITAYA_") {
			t.Fatalf("clean environment retained %q", value)
		}
	}
	found := false
	for _, value := range testServerEnvironment(false) {
		if value == "PITAYA_BENCHMARK_UNSAFE_OVERRIDE=enabled" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("compatible environment unexpectedly removed PITAYA_* values")
	}
}

func TestStartTestServerRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config TestServerConfig
		want   string
	}{
		{name: "missing type", config: TestServerConfig{}, want: "type is required"},
		{name: "negative port", config: TestServerConfig{ServerType: "game", Port: -1}, want: "invalid test server port"},
		{name: "large port", config: TestServerConfig{ServerType: "game", Port: 65536}, want: "invalid test server port"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := StartTestServer(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("StartTestServer() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestStartTestServerContextHonorsPreCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("PITAYA_TEST_SERVER_BINARY", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := StartTestServerContext(ctx, TestServerConfig{ServerType: "game"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartTestServerContext() error = %v, want context cancellation", err)
	}
}

func TestStartReadyProcessContextStopsOnCancel(t *testing.T) {
	startedFile := filepath.Join(t.TempDir(), "started")
	cmd := managedProcessCommand("cancel")
	cmd.Env = append(cmd.Env, "PITAYA_MANAGED_PROCESS_STARTED_FILE="+startedFile)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := startReadyProcessContext(ctx, cmd, serverReadyMarker, time.Second)
		result <- err
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
waitForStart:
	for {
		select {
		case err := <-result:
			t.Fatalf("startReadyProcessContext() returned before cancellation: %v", err)
		case <-ticker.C:
			if _, err := os.Stat(startedFile); err == nil {
				break waitForStart
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		case <-timer.C:
			t.Fatal("managed process did not report startup")
		}
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startReadyProcessContext() error = %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startReadyProcessContext() did not stop the canceled process")
	}
}

func TestParseTestServerPort(t *testing.T) {
	tests := []struct {
		address string
		want    int
	}{
		{address: "127.0.0.1:43210", want: 43210},
		{address: "[::]:1234", want: 1234},
		{address: "missing-port"},
		{address: "127.0.0.1:0"},
		{address: "127.0.0.1:not-a-port"},
		{address: "127.0.0.1:65536"},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			got, err := parseTestServerPort(test.address)
			if test.want == 0 {
				if err == nil {
					t.Fatalf("parseTestServerPort(%q) unexpectedly returned %d", test.address, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseTestServerPort(%q) = %d, %v; want %d", test.address, got, err, test.want)
			}
		})
	}
}

func TestCleanupStartedTestServerPropagatesStopFailure(t *testing.T) {
	cmd := managedProcessCommand("exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("helper process unexpectedly exited successfully")
	}
	done := make(chan struct{})
	close(done)
	process := &managedProcess{cmd: cmd, done: done, waitErr: waitErr}

	err := cleanupStartedTestServer(process, t.TempDir())
	if !errors.Is(err, waitErr) {
		t.Fatalf("cleanupStartedTestServer() error = %v, want wrapping %v", err, waitErr)
	}
}

func TestWithCleanupErrorPreservesPrimaryAndCleanupErrors(t *testing.T) {
	primaryErr := errors.New("startup failed")
	cleanupErr := errors.New("cleanup failed")
	err := withCleanupError(primaryErr, cleanupErr)
	if !errors.Is(err, primaryErr) {
		t.Fatalf("withCleanupError() error = %v, want primary error", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("withCleanupError() error = %v, want cleanup error", err)
	}
}

func TestWaitForTestServerReadyReportsEarlyExit(t *testing.T) {
	wantErr := errors.New("server crashed")
	process := &managedProcess{done: make(chan struct{}), waitErr: wantErr}
	close(process.done)

	_, err := waitForTestServerReady(process, filepath.Join(t.TempDir(), "ready"), time.Second)
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForTestServerReady() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestWaitForTestServerReadyReportsReadFailure(t *testing.T) {
	process := &managedProcess{done: make(chan struct{})}
	_, err := waitForTestServerReady(process, t.TempDir(), time.Second)
	if err == nil {
		t.Fatal("waitForTestServerReady() unexpectedly succeeded for a directory")
	}
}

func TestWaitForTestServerReadyReportsTimeout(t *testing.T) {
	process := &managedProcess{done: make(chan struct{})}
	_, err := waitForTestServerReady(process, filepath.Join(t.TempDir(), "missing"), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "readiness file timed out") {
		t.Fatalf("waitForTestServerReady() error = %v, want readiness timeout", err)
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

func TestStartReadyProcessReportsCleanEarlyExit(t *testing.T) {
	_, err := startReadyProcess(managedProcessCommand("clean-exit"), serverReadyMarker, 5*time.Second)
	if err == nil {
		t.Fatal("startReadyProcess() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "process exited before readiness") || strings.Contains(err.Error(), "%!w") {
		t.Fatalf("startReadyProcess() error = %q, want clean early-exit diagnostic", err)
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
