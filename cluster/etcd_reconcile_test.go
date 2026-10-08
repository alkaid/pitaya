package cluster

import (
	"context"
	"errors"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"sync/atomic"
	"testing"
	"time"
)

type quietWatcher struct{ clientv3.Watcher }

func (w quietWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	out := make(chan clientv3.WatchResponse)
	in := w.Watcher.Watch(ctx, key, opts...)
	go func() {
		defer close(out)
		for range in {
		}
	}()
	return out
}

func TestSyncServersRepairsMissedExistingPut(t *testing.T) {
	cluster, cli := helpers.GetTestEtcd(t)
	defer cluster.Terminate(t)
	cli.Watcher = quietWatcher{cli.Watcher}
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Prefix = ""
	self := NewServer("self", "frontend", true)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	defer sd.Shutdown()
	sv := NewServer("peer", "backend", false, map[string]string{"version": "one"})
	_, err := cli.Put(context.Background(), getKey(sv.ID, sv.Type), sv.AsJSONString())
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(false))
	sv.Metadata["version"] = "two"
	_, err = cli.Put(context.Background(), getKey(sv.ID, sv.Type), sv.AsJSONString())
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(false))
	got, err := sd.GetServer(sv.ID)
	require.NoError(t, err)
	require.Equal(t, "two", got.Metadata["version"])
}

type reconcileListener struct {
	check func(Action, *Server, *Server)
}

func (l reconcileListener) AddServer(s *Server)         { l.check(ADD, s, nil) }
func (l reconcileListener) RemoveServer(s *Server)      { l.check(DEL, s, nil) }
func (l reconcileListener) ModifyServer(s, old *Server) { l.check(Modify, s, old) }

type scriptedKV struct {
	clientv3.KV
	get func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
}

func (k scriptedKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	return k.get(ctx, key, opts...)
}

type scriptedWatcher struct {
	clientv3.Watcher
	watch func(context.Context, string, ...clientv3.OpOption) clientv3.WatchChan
}

func (w scriptedWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	return w.watch(ctx, key, opts...)
}

func watchPut(s *Server, rev int64) *clientv3.Event {
	return &clientv3.Event{Type: clientv3.EventTypePut, Kv: &mvccpb.KeyValue{Key: []byte(getKey(s.ID, s.Type)), Value: []byte(s.AsJSONString()), ModRevision: rev}}
}

func TestReconcileRevisionBoundaryAndReadableCallbacks(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("self", "frontend", true), nil)
	first := NewServer("a", "backend", false, map[string]string{"v": "old"})
	second := NewServer("b", "backend", false)
	sd.appliedRevision = 10
	sd.applyWatchResponse(clientv3.WatchResponse{Header: etcdserverpb.ResponseHeader{Revision: 99}, Created: true})
	require.Equal(t, int64(10), sd.appliedRevision)
	var actions []Action
	sd.AddListener(reconcileListener{check: func(action Action, s, old *Server) {
		actions = append(actions, action)
		servers, err := sd.GetServersByType("backend")
		require.NoError(t, err)
		require.Len(t, servers, 2, "entire transaction must commit before callbacks")
		require.Len(t, sd.GetServers(), 2)
		require.Len(t, sd.GetServerTypes(), 1)
		_, err = sd.GetConsistentHashNode("backend", "session")
		require.NoError(t, err)
	}})
	sd.applyWatchResponse(clientv3.WatchResponse{Events: []*clientv3.Event{watchPut(first, 11), watchPut(second, 11)}})
	require.Equal(t, []Action{ADD, ADD}, actions)
	require.Equal(t, int64(11), sd.appliedRevision)
	sd.listenerMu.Lock()
	sd.listeners = nil
	sd.listenerMu.Unlock()
	newer := NewServer("a", "backend", false, map[string]string{"v": "new"})
	sd.cli = &clientv3.Client{KV: scriptedKV{get: func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: 20}, Kvs: []*mvccpb.KeyValue{watchPut(newer, 20).Kv}}, nil
	}}}
	require.NoError(t, sd.SyncServers(false))
	// The old queued PUT may neither overwrite a newer snapshot nor resurrect b.
	sd.applyWatchResponse(clientv3.WatchResponse{Events: []*clientv3.Event{watchPut(first, 15), watchPut(second, 15)}})
	got, err := sd.GetServer("a")
	require.NoError(t, err)
	require.Equal(t, "new", got.Metadata["v"])
	_, err = sd.GetServer("b")
	require.Error(t, err)
	sd.cli.KV = scriptedKV{get: func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
		return nil, errors.New("unavailable")
	}}
	require.Error(t, sd.SyncServers(false))
	require.Len(t, sd.GetServers(), 1)
	sd.applyWatchResponse(clientv3.WatchResponse{Events: []*clientv3.Event{{Type: clientv3.EventTypePut, Kv: &mvccpb.KeyValue{Key: []byte("servers/backend/a"), Value: []byte("null"), ModRevision: 21}}}})
	got, err = sd.GetServer("a")
	require.NoError(t, err)
	require.Equal(t, "new", got.Metadata["v"])
	sd.applyWatchResponse(clientv3.WatchResponse{Events: []*clientv3.Event{watchPut(first, 22)}})
	got, err = sd.GetServer("a")
	require.NoError(t, err)
	require.Equal(t, "old", got.Metadata["v"])
}

func TestWatchRecoveryAfterClosureCompactionAndGetFailures(t *testing.T) {
	for _, failure := range []string{"close", "compaction", "snapshot-window"} {
		t.Run(failure, func(t *testing.T) {
			cluster, cli := helpers.GetTestEtcd(t)
			defer cluster.Terminate(t)
			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			cfg.Prefix = ""
			sd := getEtcdSD(t, cfg, NewServer("self", "frontend", true), cli)
			defer sd.Shutdown()
			kv := cli.KV
			watcher := cli.Watcher
			peer := NewServer("peer", "backend", false, map[string]string{"v": "one"})
			_, err := kv.Put(context.Background(), getKey(peer.ID, peer.Type), peer.AsJSONString())
			require.NoError(t, err)
			var failGets atomic.Int32
			cli.KV = scriptedKV{KV: kv, get: func(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
				if failGets.Load() > 0 {
					failGets.Add(-1)
					return nil, errors.New("injected snapshot failure")
				}
				return kv.Get(ctx, key, opts...)
			}}
			var calls atomic.Int32
			ready := make(chan struct{})
			cli.Watcher = scriptedWatcher{Watcher: watcher, watch: func(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
				if calls.Add(1) == 1 {
					// Executed strictly between the snapshot and watch subscription.
					updated := NewServer("peer", "backend", false, map[string]string{"v": "two"})
					_, err := kv.Put(ctx, getKey(peer.ID, peer.Type), updated.AsJSONString())
					if err != nil {
						panic(err)
					}
					if failure != "snapshot-window" {
						failGets.Store(2)
						ch := make(chan clientv3.WatchResponse, 1)
						if failure == "compaction" {
							ch <- clientv3.WatchResponse{Canceled: true, CompactRevision: 100}
						}
						close(ch)
						return ch
					}
				}
				ch := watcher.Watch(ctx, key, opts...)
				close(ready)
				return ch
			}}
			sd.loops.Go(sd.watchEtcdChanges)
			select {
			case <-ready:
			case <-time.After(8 * time.Second):
				t.Fatal("watch never recovered")
			}
			require.Eventually(t, func() bool { got, err := sd.GetServer("peer"); return err == nil && got.Metadata["v"] == "two" }, time.Second, time.Millisecond)
			next := NewServer("peer", "backend", false, map[string]string{"v": "three"})
			other := NewServer("other", "backend", false)
			_, err = kv.Txn(context.Background()).Then(clientv3.OpPut(getKey(next.ID, next.Type), next.AsJSONString()), clientv3.OpPut(getKey(other.ID, other.Type), other.AsJSONString())).Commit()
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				got, err := sd.GetServer("peer")
				_, otherErr := sd.GetServer("other")
				return err == nil && otherErr == nil && got.Metadata["v"] == "three"
			}, time.Second, time.Millisecond)
			_, err = kv.Delete(context.Background(), getKey(other.ID, other.Type))
			require.NoError(t, err)
			require.Eventually(t, func() bool { _, err := sd.GetServer("other"); return err != nil }, time.Second, time.Millisecond)
		})
	}
}

func TestShutdownJoinsExternalSync(t *testing.T) {
	peer := NewServer("peer", "backend", false)
	cli := &clientv3.Client{KV: scriptedKV{get: func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
		return &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: 1}, Kvs: []*mvccpb.KeyValue{watchPut(peer, 1).Kv}}, nil
	}}}
	service, err := NewEtcdServiceDiscovery(config.NewDefaultPitayaConfig().Cluster.SD.Etcd, NewServer("self", "frontend", true), nil, cli)
	if err != nil {
		t.Fatal(err)
	}
	sd := service.(*etcdServiceDiscovery)
	entered, release, syncDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var shutdownReturned, late atomic.Bool
	sd.AddListener(reconcileListener{check: func(Action, *Server, *Server) { close(entered); <-release }})
	sd.AddListener(reconcileListener{check: func(Action, *Server, *Server) { late.Store(shutdownReturned.Load()) }})
	go func() {
		defer close(syncDone)
		if err := sd.SyncServers(false); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { _ = sd.Shutdown(); shutdownReturned.Store(true); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-syncDone
	<-stopped
	if late.Load() {
		t.Fatal("Shutdown returned while an external SyncServers callback batch was active; the next listener ran after Shutdown")
	}
}
