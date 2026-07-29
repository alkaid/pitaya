package helpers

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	gnatsd "github.com/nats-io/nats-server/v2/test"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/integration"
)

// GetFreePort returns a free port
func GetFreePort(t testing.TB) int {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// GetMapKeys returns a string slice with the map keys
func GetMapKeys(t *testing.T, m interface{}) []string {
	if reflect.ValueOf(m).Kind() != reflect.Map {
		t.Fatal(errors.New("GetMapKeys should receive a map"))
	}
	if reflect.TypeOf(m).Key() != reflect.TypeOf("bla") {
		t.Fatal(errors.New("GetMapKeys should receive a map with string keys"))
	}
	t.Helper()
	res := make([]string, 0)
	for _, k := range reflect.ValueOf(m).MapKeys() {
		res = append(res, k.String())
	}
	return res
}

// GetTestNatsServer gets a test nats server
func GetTestNatsServer(t *testing.T) *server.Server {
	opts := gnatsd.DefaultTestOptions
	port := GetFreePort(t)
	opts.Port = port
	s := gnatsd.RunServer(&opts)
	return s
}

// GetTestEtcd gets a test in memory etcd server
func GetTestEtcd(t *testing.T) (*integration.Cluster, *clientv3.Client) {
	t.Helper()
	integration.BeforeTestExternal(t)
	c := integration.NewCluster(t, &integration.ClusterConfig{Size: 1})
	cli := c.RandClient()
	return c, cli
}

// WriteFile test helper
func WriteFile(t *testing.T, filepath string, bytes []byte) {
	t.Helper()
	if err := ioutil.WriteFile(filepath, bytes, 0644); err != nil {
		t.Fatalf("failed writing file: %s", err)
	}
}

// ReadFile test helper
func ReadFile(t *testing.T, filepath string) []byte {
	t.Helper()
	b, err := ioutil.ReadFile(filepath)
	if err != nil {
		t.Fatalf("failed reading file: %s", err)
	}
	return b
}

// StartProcess starts a process
func StartProcess(t testing.TB, program string, args ...string) *exec.Cmd {
	t.Helper()
	return exec.Command(program, args...)
}

const (
	serverReadyMarker         = "all modules started!"
	processOutputPendingLimit = 64 * 1024
	processWaitDelay          = 2 * time.Second
	processTerminateTimeout   = 5 * time.Second
	processKillWaitTimeout    = 3 * time.Second
)

type processOutput struct {
	mu        sync.Mutex
	pending   string
	lines     []string
	limit     int
	marker    string
	ready     chan struct{}
	readyOnce sync.Once
}

func newProcessOutput(marker string, limit int) *processOutput {
	return &processOutput{marker: marker, limit: limit, ready: make(chan struct{})}
}

func (o *processOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.pending += string(data)
	for {
		newline := strings.IndexByte(o.pending, '\n')
		if newline < 0 {
			break
		}
		o.appendLine(strings.TrimSuffix(o.pending[:newline], "\r"))
		o.pending = o.pending[newline+1:]
	}
	if strings.Contains(o.pending, o.marker) {
		o.readyOnce.Do(func() { close(o.ready) })
	}
	if len(o.pending) > processOutputPendingLimit {
		o.pending = o.pending[len(o.pending)-processOutputPendingLimit:]
	}
	return len(data), nil
}

func (o *processOutput) appendLine(line string) {
	if strings.Contains(line, o.marker) {
		o.readyOnce.Do(func() { close(o.ready) })
	}
	if len(line) > processOutputPendingLimit {
		line = line[len(line)-processOutputPendingLimit:]
	}
	o.lines = append(o.lines, line)
	if len(o.lines) > o.limit {
		o.lines = append([]string(nil), o.lines[len(o.lines)-o.limit:]...)
	}
}

func (o *processOutput) tail() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	lines := append([]string(nil), o.lines...)
	if o.pending != "" {
		lines = append(lines, o.pending)
	}
	if len(lines) > o.limit {
		lines = lines[len(lines)-o.limit:]
	}
	return strings.Join(lines, "\n")
}

type managedProcess struct {
	cmd      *exec.Cmd
	control  *processControl
	done     chan struct{}
	output   *processOutput
	waitErr  error
	stopOnce sync.Once
	stopErr  error
}

func startReadyProcess(cmd *exec.Cmd, marker string, readyTimeout time.Duration) (*managedProcess, error) {
	output := newProcessOutput(marker, 200)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = processWaitDelay
	prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	control, err := newProcessControl(cmd.Process)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("manage process tree: %w", err)
	}

	process := &managedProcess{cmd: cmd, control: control, done: make(chan struct{}), output: output}
	go func() {
		process.waitErr = cmd.Wait()
		_ = control.release()
		close(process.done)
	}()

	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case <-output.ready:
		return process, nil
	case <-process.done:
		return nil, fmt.Errorf("process exited before readiness: %w\nlast output:\n%s", process.waitErr, output.tail())
	case <-timer.C:
		_ = process.Stop()
		return nil, fmt.Errorf("process readiness timed out after %s\nlast output:\n%s", readyTimeout, output.tail())
	}
}

func (p *managedProcess) Stop() error {
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
			p.recordWaitError(false, false)
			return
		default:
		}

		stopRequested := false
		hardKill, err := p.control.terminate()
		if err == nil {
			stopRequested = true
		} else if errors.Is(err, os.ErrProcessDone) {
			if !p.waitForDone(processKillWaitTimeout) {
				p.stopErr = fmt.Errorf("process wait timed out after %s", processKillWaitTimeout)
				return
			}
			p.recordWaitError(false, false)
			return
		} else {
			p.stopErr = err
		}
		if hardKill {
			if err != nil {
				return
			}
			if !p.waitForDone(processKillWaitTimeout) {
				p.stopErr = fmt.Errorf("process wait timed out after hard kill after %s", processKillWaitTimeout)
				return
			}
			p.recordWaitError(stopRequested, true)
			return
		}

		timer := time.NewTimer(processTerminateTimeout)
		defer timer.Stop()
		select {
		case <-p.done:
			p.recordWaitError(stopRequested, false)
			return
		case <-timer.C:
			killErr := p.control.kill()
			if killErr == nil {
				stopRequested = true
				hardKill = true
			} else if !errors.Is(killErr, os.ErrProcessDone) {
				if p.stopErr == nil {
					p.stopErr = killErr
				}
				return
			}
			if !p.waitForDone(processKillWaitTimeout) {
				if p.stopErr == nil {
					p.stopErr = fmt.Errorf("process wait timed out after hard kill after %s", processKillWaitTimeout)
				}
				return
			}
			p.recordWaitError(stopRequested, hardKill)
		}
	})
	return p.stopErr
}

func (p *managedProcess) waitForDone(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func (p *managedProcess) recordWaitError(stopRequested, hardKill bool) {
	if p.waitErr == nil || p.stopErr != nil {
		return
	}
	if stopRequested && isExpectedProcessExit(p.cmd.ProcessState, hardKill) {
		return
	}
	p.stopErr = p.waitErr
}

// StartServer starts a server
func StartServer(
	t testing.TB,
	frontend, debug bool,
	svType string,
	port int,
	sdPrefix string,
	grpc, lazyConnection bool,
) func() {
	grpcPort := GetFreePort(t)
	promPort := GetFreePort(t)
	var useGRPC string
	if grpc {
		useGRPC = "true"
	} else {
		useGRPC = "false"
	}
	t.Helper()
	args := []string{
		"-type",
		svType,
		"-port",
		strconv.Itoa(port),
		fmt.Sprintf("-frontend=%s", strconv.FormatBool(frontend)),
		"-sdprefix", sdPrefix,
		"-grpcport", fmt.Sprintf("%d", grpcPort),
		fmt.Sprintf("-grpc=%s", useGRPC),
	}
	if debug {
		args = append(args, "-debug")
	}
	cmd := StartProcess(
		t,
		serverBinary(),
		args...,
	)

	// always use a random port for prometheus, to avoid e2e conflicts
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PITAYA_METRICS_PROMETHEUS_PORT=%d", promPort),
		fmt.Sprintf("PITAYA_CLUSTER_RPC_CLIENT_GRPC_LAZYCONNECTION=%v", lazyConnection),
	)
	if value := os.Getenv("PITAYA_TEST_NATS_URL"); value != "" {
		cmd.Args = append(cmd.Args, "-natsurl", value)
	}
	if value := os.Getenv("PITAYA_TEST_ETCD_URL"); value != "" {
		cmd.Args = append(cmd.Args, "-etcdurl", value)
	}
	if value := os.Getenv("PITAYA_TEST_REDIS_URL"); value != "" {
		cmd.Args = append(cmd.Args, "-redisurl", value)
	}

	process, err := startReadyProcess(cmd, serverReadyMarker, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := process.Stop(); err != nil {
			t.Errorf("stop test server: %v\nlast output:\n%s", err, process.output.tail())
		}
	}
}

func serverBinary() string {
	if binary := os.Getenv("PITAYA_TEST_SERVER_BINARY"); binary != "" {
		return binary
	}
	return "../examples/testing/server"
}

// FixtureGoldenFileName returns the golden file name on fixtures path
func FixtureGoldenFileName(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("fixtures", name+".golden")
}

func vetExtras(extras []interface{}) (bool, string) {
	for i, extra := range extras {
		if extra != nil {
			zeroValue := reflect.Zero(reflect.TypeOf(extra)).Interface()
			if !reflect.DeepEqual(zeroValue, extra) {
				message := fmt.Sprintf("unexpected non-nil/non-zero extra argument at index %d:\n\t<%T>: %#v", i+1, extra, extra)
				return false, message
			}
		}
	}
	return true, ""
}

func pollFuncReturn(f interface{}) (interface{}, error) {
	values := reflect.ValueOf(f).Call([]reflect.Value{})

	extras := []interface{}{}
	for _, value := range values[1:] {
		extras = append(extras, value.Interface())
	}

	success, message := vetExtras(extras)

	if !success {
		return nil, errors.New(message)
	}

	return values[0].Interface(), nil
}

// ShouldEventuallyReceive should asserts that eventually channel c receives a value
func ShouldEventuallyReceive(t testing.TB, c interface{}, timeouts ...time.Duration) interface{} {
	t.Helper()
	if !isChan(c) {
		t.Fatal("ShouldEventuallyReceive c argument should be a channel")
	}
	v := reflect.ValueOf(c)

	timeout := time.After(500 * time.Millisecond)

	if len(timeouts) > 0 {
		timeout = time.After(timeouts[0])
	}

	recvChan := make(chan reflect.Value)

	go func() {
		v, ok := v.Recv()
		if ok {
			recvChan <- v
		}
	}()

	select {
	case <-timeout:
		t.Fatal(errors.New("timed out waiting for channel to receive"))
	case a := <-recvChan:
		return a.Interface()
	}

	return nil
}

// ShouldAlwaysReturn asserts that the return of f should always be v, timeouts: 0 - evaluation interval, 1 - timeout
func ShouldAlwaysReturn(t testing.TB, f interface{}, v interface{}, timeouts ...time.Duration) {
	t.Helper()
	interval := 10 * time.Millisecond
	timeout := time.After(50 * time.Millisecond)
	switch len(timeouts) {
	case 1:
		interval = timeouts[0]
		break
	case 2:
		interval = timeouts[0]
		timeout = time.After(timeouts[1])
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if isFunction(f) {
		for {
			select {
			case <-timeout:
				return
			case <-ticker.C:
				val, err := pollFuncReturn(f)
				if err != nil {
					t.Fatal(err)
				}
				if v != val {
					t.Fatalf("function f returned wrong value %s", val)
				}
			}
		}
	} else {
		t.Fatal("ShouldAlwaysReturn should receive a function with no args and more than 0 outs")
		return
	}
}

// ShouldEventuallyReturn asserts that eventually the return of f should be v, timeouts: 0 - evaluation interval, 1 - timeout
func ShouldEventuallyReturn(t testing.TB, f interface{}, v interface{}, timeouts ...time.Duration) {
	t.Helper()
	interval := 10 * time.Millisecond
	timeout := time.After(500 * time.Millisecond)
	switch len(timeouts) {
	case 1:
		interval = timeouts[0]
		break
	case 2:
		interval = timeouts[0]
		timeout = time.After(timeouts[1])
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if isFunction(f) {
		for {
			select {
			case <-timeout:
				t.Fatalf("function f never returned value %s", v)
			case <-ticker.C:
				val, err := pollFuncReturn(f)
				if err != nil {
					t.Fatal(err)
				}
				if v == val {
					return
				}
			}
		}
	} else {
		t.Fatal("ShouldEventuallyEqual should receive a function with no args and more than 0 outs")
		return
	}
}
