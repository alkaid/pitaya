package helpers

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func waitForServerToBeReady(t testing.TB, out *bufio.Reader) {
	t.Helper()
	ready := make(chan error, 1)
	go func() {
		for {
			line, _, err := out.ReadLine()
			if err != nil {
				ready <- err
				return
			}
			if strings.Contains(string(line), "all modules started!") {
				ready <- nil
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("test server exited before ready: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("test server did not become ready")
	}
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
	for env, flag := range map[string]string{
		"PITAYA_TEST_ETCD_ENDPOINT": "-etcdurl",
		"PITAYA_TEST_REDIS_ADDR":    "-redisurl",
		"PITAYA_TEST_NATS_URL":      "-natsurl",
	} {
		if value := os.Getenv(env); value != "" {
			args = append(args, flag, value)
		}
	}
	cmd := StartProcess(
		t,
		"../examples/testing/server",
		args...,
	)

	// always use a random port for prometheus, to avoid e2e conflicts
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PITAYA_METRICS_PROMETHEUS_PORT=%d", promPort),
		fmt.Sprintf("PITAYA_CLUSTER_RPC_CLIENT_GRPC_LAZYCONNECTION=%v", lazyConnection),
	)

	outPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			if runtime.GOOS == "windows" {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("test server shutdown: %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("test server failed to stop on SIGTERM")
			}
		})
	}
	t.Cleanup(stop)
	reader := bufio.NewReader(outPipe)
	waitForServerToBeReady(t, reader)
	go io.Copy(io.Discard, reader)
	return stop
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
