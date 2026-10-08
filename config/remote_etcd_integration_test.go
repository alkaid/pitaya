//go:build integration

package config

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Cut the application's real gRPC connections while keeping the administrative
// client connected. This makes writes and compaction during an outage explicit.
type configEtcdProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	paused   bool
	pairs    map[net.Conn]net.Conn
	done     chan struct{}
	workers  sync.WaitGroup
}

func newConfigEtcdProxy(t *testing.T, target string) *configEtcdProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &configEtcdProxy{listener: listener, target: strings.TrimPrefix(target, "http://"), pairs: make(map[net.Conn]net.Conn), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			p.workers.Go(func() { p.forward(conn) })
		}
	}()
	t.Cleanup(func() { listener.Close(); <-p.done; p.pause(); p.workers.Wait() })
	return p
}
func (p *configEtcdProxy) forward(conn net.Conn) {
	defer conn.Close()
	backend, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer backend.Close()
	p.mu.Lock()
	if p.paused {
		p.mu.Unlock()
		return
	}
	p.pairs[conn] = backend
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pairs, conn); p.mu.Unlock() }()
	copied := make(chan struct{})
	go func() { io.Copy(backend, conn); backend.Close(); close(copied) }()
	io.Copy(conn, backend)
	conn.Close()
	<-copied
}
func (p *configEtcdProxy) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = true
	for conn, backend := range p.pairs {
		conn.Close()
		backend.Close()
	}
}
func (p *configEtcdProxy) resume() { p.mu.Lock(); p.paused = false; p.mu.Unlock() }

// PITAYA_TEST_ETCD_ENDPOINT must be a disposable etcd instance: this test
// compacts global history. Missing infrastructure fails instead of skipping.
func TestRemoteConfigEtcdRecoveryIntegration(t *testing.T) {
	endpoint := os.Getenv("PITAYA_TEST_ETCD_ENDPOINT")
	require.NotEmpty(t, endpoint, "set PITAYA_TEST_ETCD_ENDPOINT to a disposable etcd")
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { cli.Close() })
	prefix := fmt.Sprintf("/pitaya-config-integration/%d/", time.Now().UnixNano())
	low, high := prefix+"low", prefix+"high"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := cli.Delete(cleanup, prefix, clientv3.WithPrefix())
		require.NoError(t, err)
	})
	put := func(key, value string) int64 {
		t.Helper()
		res, err := cli.Put(ctx, key, value)
		require.NoError(t, err)
		return res.Header.Revision
	}
	put(low, `{"feature":{"value":"low","fields":{"low":"one"}}}`)
	put(high, `{"feature":{"value":"initial","removed":"old","fields":{"high":"one"}}}`)
	proxy := newConfigEtcdProxy(t, endpoint)
	c := NewConfig()
	c.Set("pitaya.confsource.etcd.keys", []string{low, high})
	c.Set("pitaya.confsource.etcd.endpoints", []string{proxy.listener.Addr().String()})
	c.Set("pitaya.confsource.formatter", "json")
	updates, _ := recordFeature(c)
	require.NoError(t, c.InitLoad())
	awaitFeature(t, updates, "initial")
	// A save after InitLoad but before starting the watch must be observed.
	put(high, `{"feature":{"value":"startup-gap"}}`)
	module := NewConfigModule(c)
	t.Cleanup(func() { require.NoError(t, module.Shutdown()) })
	module.AfterInit()
	awaitFeature(t, updates, "startup-gap")
	for i := 0; i < 5; i++ {
		value := fmt.Sprintf("put-%d", i)
		put(high, fmt.Sprintf(`{"feature":{"value":%q}}`, value))
		awaitFeature(t, updates, value)
	}
	// Invalid data must not take down the watch; a following valid PUT recovers.
	put(high, `{"feature":`)
	put(high, `{"feature":{"value":"valid-again"}}`)
	awaitFeature(t, updates, "valid-again")
	_, err = cli.Delete(ctx, high)
	require.NoError(t, err)
	put(low, `{"feature":{"value":"lower","fields":{"low":"two"}}}`)
	retained := awaitFeature(t, updates, "valid-again")
	require.Equal(t, "two", retained.Fields["low"], "missing high key keeps its last good value while low keeps watching")
	put(high, `{"feature":{"value":"recreated","removed":"old","fields":{"high":"new"}}}`)
	awaitFeature(t, updates, "recreated")
	put(high, `{}`)
	cleared := awaitFeature(t, updates, "lower")
	require.Empty(t, cleared.Removed)
	require.Equal(t, map[string]string{"low": "two"}, cleared.Fields)
	// Break both shared-client watches, save repeatedly and compact while the
	// application is disconnected, then verify catch-up and subsequent PUTs.
	proxy.pause()
	put(high, `{"feature":{"value":"during-outage-1"}}`)
	revision := put(high, `{"feature":{"value":"during-outage-2"}}`)
	_, err = cli.Compact(ctx, revision, clientv3.WithCompactPhysical())
	require.NoError(t, err)
	proxy.resume()
	awaitFeature(t, updates, "during-outage-2")
	put(high, `{"feature":{"value":"after-recovery"}}`)
	awaitFeature(t, updates, "after-recovery")
	require.NoError(t, module.Shutdown())
	require.NoError(t, module.Shutdown())
	for len(updates) > 0 {
		<-updates
	}
	put(high, `{"feature":{"value":"after-shutdown"}}`)
	select {
	case update := <-updates:
		t.Fatalf("callback after Shutdown: %+v", update)
	case <-time.After(100 * time.Millisecond):
	}
}
