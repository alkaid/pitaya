// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cluster

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/logger"
	logutil "go.etcd.io/etcd/client/pkg/v3/logutil"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
	"google.golang.org/grpc"
)

var after = time.After

type etcdLifecycleState uint8

const (
	etcdLifecycleNew etcdLifecycleState = iota
	etcdLifecycleInitializing
	etcdLifecycleRunning
	etcdLifecycleStopping
	etcdLifecycleStopped
)

func (sd *etcdServiceDiscovery) startLifecycleWorker(worker func()) bool {
	sd.lifecycle.mu.Lock()
	defer sd.lifecycle.mu.Unlock()
	// lifecycleLock is the admission gate pairing WaitGroup.Add with Shutdown's
	// transition to stopping. Once Shutdown can Wait, no new worker can Add.
	// Lifecycle workers may enqueue callbacks, but must not wait for callback
	// completion or a listener calling Shutdown would wait on its own worker.
	if (sd.lifecycle.state != etcdLifecycleInitializing && sd.lifecycle.state != etcdLifecycleRunning) || sd.lifecycle.ctx.Err() != nil {
		return false
	}
	sd.lifecycle.wg.Add(1)
	go func() {
		defer sd.lifecycle.wg.Done()
		worker()
	}()
	return true
}

func (sd *etcdServiceDiscovery) newOwnedClient() (*clientv3.Client, error) {
	logger.Zap.Info("Initializing ETCD client")
	etcdClientLogger, _ := logutil.CreateDefaultZapLogger(logutil.ConvertToZapLevel("error"))
	clientTLS, err := config.LoadClientTLS("pitaya.cluster.sd.etcd.tls", sd.etcdTLS)
	if err != nil {
		logger.Zap.Error("error loading etcd client TLS configuration", zap.Error(err))
		return nil, err
	}
	clientConfig := clientv3.Config{
		Endpoints:   sd.etcdEndpoints,
		DialTimeout: sd.etcdDialTimeout,
		Logger:      etcdClientLogger,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
		TLS:         clientTLS,
		Username:    sd.etcdUser,
		Password:    sd.etcdPass,
	}
	cli, err := sd.newEtcdClient(clientConfig)
	if err != nil {
		logger.Zap.Error("error initializing etcd client", zap.Error(err))
		return nil, err
	}
	return cli, nil
}

func (sd *etcdServiceDiscovery) namespaceClient(cli *clientv3.Client) {
	cli.KV = namespace.NewKV(cli.KV, sd.etcdPrefix)
	cli.Watcher = namespace.NewWatcher(cli.Watcher, sd.etcdPrefix)
	cli.Lease = namespace.NewLease(cli.Lease, sd.etcdPrefix)
}

func (sd *etcdServiceDiscovery) initClient() error {
	sd.lifecycle.mu.Lock()
	cli := sd.cli
	initializing := sd.lifecycle.state == etcdLifecycleInitializing
	sd.lifecycle.mu.Unlock()
	if !initializing {
		return context.Canceled
	}
	if cli != nil {
		sd.namespaceClient(cli)
		return nil
	}

	candidate, err := sd.newOwnedClient()
	if err != nil {
		return err
	}
	sd.namespaceClient(candidate)
	sd.lifecycle.mu.Lock()
	if sd.lifecycle.state != etcdLifecycleInitializing {
		sd.lifecycle.mu.Unlock()
		_ = candidate.Close()
		return context.Canceled
	}
	sd.cli = candidate
	sd.ownsClient = true
	sd.lifecycle.mu.Unlock()
	return nil
}

// Init starts the service discovery client
func (sd *etcdServiceDiscovery) Init() error {
	sd.lifecycle.mu.Lock()
	switch sd.lifecycle.state {
	case etcdLifecycleNew:
		sd.lifecycle.state = etcdLifecycleInitializing
		sd.lifecycle.initDone = make(chan struct{})
	case etcdLifecycleInitializing:
		initDone := sd.lifecycle.initDone
		sd.lifecycle.mu.Unlock()
		<-initDone
		sd.lifecycle.mu.Lock()
		err := sd.lifecycle.initErr
		sd.lifecycle.mu.Unlock()
		return err
	case etcdLifecycleRunning:
		sd.lifecycle.mu.Unlock()
		return nil
	case etcdLifecycleStopping:
		initDone := sd.lifecycle.initDone
		if initDone != nil {
			select {
			case <-initDone:
			default:
				sd.lifecycle.mu.Unlock()
				<-initDone
				sd.lifecycle.mu.Lock()
				err := sd.lifecycle.initErr
				sd.lifecycle.mu.Unlock()
				return err
			}
		}
		fallthrough
	case etcdLifecycleStopped:
		err := sd.lifecycle.initErr
		if err == nil {
			err = sd.lifecycle.ctx.Err()
		}
		if err == nil {
			err = context.Canceled
		}
		sd.lifecycle.mu.Unlock()
		return err
	}
	sd.lifecycle.mu.Unlock()

	err := sd.initOnce()
	return sd.finishInit(err)
}

func (sd *etcdServiceDiscovery) initOnce() error {
	if err := sd.initClient(); err != nil {
		return err
	}

	if err := sd.bootstrap(); err != nil {
		return err
	}
	if !sd.startLifecycleWorker(func() { sd.runRenewalSync(sd.lifecycle.ctx) }) {
		return sd.lifecycle.ctx.Err()
	}
	// Establish the initial snapshot revision before starting the watch. The
	// watcher resumes at revision+1, closing the list/watch creation window.
	if !sd.startLifecycleWorker(func() { sd.watchEtcdChanges(sd.lifecycle.ctx) }) {
		return sd.lifecycle.ctx.Err()
	}

	// update servers
	if !sd.startLifecycleWorker(func() {
		syncServersTicker := time.NewTicker(sd.watch.syncInterval)
		defer syncServersTicker.Stop()
		for {
			select {
			case <-syncServersTicker.C:
				err := sd.syncServers(sd.lifecycle.ctx, false, false)
				if err != nil && sd.lifecycle.ctx.Err() == nil {
					logger.Zap.Error("error resyncing servers", zap.Error(err))
				}
			case <-sd.lifecycle.ctx.Done():
				return
			}
		}
	}) {
		return sd.lifecycle.ctx.Err()
	}

	return nil
}

func (sd *etcdServiceDiscovery) finishInit(initErr error) error {
	sd.lifecycle.mu.Lock()
	result := initErr
	if sd.lifecycle.state != etcdLifecycleInitializing && result == nil {
		result = sd.lifecycle.ctx.Err()
		if result == nil {
			result = context.Canceled
		}
	}
	if result == nil {
		sd.lifecycle.state = etcdLifecycleRunning
		sd.lifecycle.running.Store(true)
	} else {
		sd.lifecycle.initErr = result
		if sd.lifecycle.state == etcdLifecycleInitializing {
			sd.lifecycle.state = etcdLifecycleStopping
		}
	}
	close(sd.lifecycle.initDone)
	sd.lifecycle.mu.Unlock()

	if result != nil {
		_ = sd.Shutdown()
	}
	return result
}

func (sd *etcdServiceDiscovery) Running() bool {
	return sd.lifecycle.running.Load()
}

// BeforeShutdown executes before shutting down and will remove the server from the list
func (sd *etcdServiceDiscovery) BeforeShutdown() {
	sd.lifecycle.mu.Lock()
	lifecycleState := sd.lifecycle.state
	sd.lifecycle.mu.Unlock()
	if lifecycleState < etcdLifecycleRunning {
		return
	}
	sd.lifecycle.beforeStopOnce.Do(func() {
		// Linearize the serving-to-stopping transition with every local owner epoch
		// publication. A renewal already past its etcd transaction must not reopen
		// RPC admission while accepted callbacks are still draining.
		sd.lifecycle.mu.Lock()
		if sd.lifecycle.state == etcdLifecycleRunning {
			sd.lifecycle.state = etcdLifecycleStopping
		}
		sd.server.InvalidateOwnerEpoch()
		sd.lifecycle.mu.Unlock()
		sd.stopCallbacks()
		sd.stopLeaderCallbacks()
		// A listener may be blocked in FlushServer2Cluster. Cancel Flush before
		// joining either callback lane or the drain and revoke phases could wait on
		// each other indefinitely.
		sd.lease.flushCancel()
		// BeforeShutdown is the non-reentrant lifecycle barrier: dependent modules
		// must not begin teardown while already accepted listeners in either lane
		// can still run. Shutdown intentionally does not join so a listener may
		// call it itself.
		<-sd.watch.callbacks.drainedSignal()
		<-sd.election.callbacks.drainedSignal()
		sd.watch.listenersMu.Lock()
		sd.watch.listeners = nil
		sd.watch.listenersMu.Unlock()
		sd.election.leaderMu.Lock()
		sd.election.listener = nil
		sd.election.leaderMu.Unlock()
		if !sd.Running() {
			// Shutdown is callback-safe because it does not join these drains, but it
			// also does not revoke. If it ran first, leave the registration to lease
			// expiry; normal module teardown must call BeforeShutdown first.
			return
		}
		// Cancel renewal before taking the publication gate. A renewal which already
		// owns the gate either finishes first and has its new lease revoked below, or
		// observes cancellation and cannot publish a replacement registration.
		sd.lease.cancel()
		_ = sd.revoke()
		sd.cancelElection()
		<-after(sd.shutdownDelay)
	})
}

// Shutdown executes on shutdown and will clean etcd
func (sd *etcdServiceDiscovery) Shutdown() error {
	sd.lifecycle.shutdownOnce.Do(func() {
		sd.lifecycle.running.Store(false)
		sd.lifecycle.mu.Lock()
		initializing := sd.lifecycle.state == etcdLifecycleInitializing
		initWaitingCallbacks := sd.lifecycle.waitingCallbacks.Load()
		if sd.lifecycle.state < etcdLifecycleStopping {
			sd.lifecycle.state = etcdLifecycleStopping
		}
		// Publish and invalidation share lifecycleLock, so no renewal can make the
		// local owner valid between this transition and the shutdown barrier.
		sd.server.InvalidateOwnerEpoch()
		initDone := sd.lifecycle.initDone
		sd.lifecycle.mu.Unlock()

		// Stop worker and both callback admissions before cancellation and Wait.
		// This ordering is the shutdown gate documented above each subsystem.
		sd.stopCallbacks()
		sd.stopLeaderCallbacks()
		sd.lease.flushCancel()
		sd.lease.cancel()
		sd.lifecycle.cancel()
		sd.cancelElection()
		// Init may be waiting for an initial listener which calls Shutdown. Only
		// that reentrant window skips the join; other initialization races retain
		// Shutdown's historical completion boundary.
		if initDone != nil && (!initializing || !initWaitingCallbacks) {
			<-initDone
		}

		sd.lifecycle.wg.Wait()
		sd.lifecycle.mu.Lock()
		cli := sd.cli
		ownsClient := sd.ownsClient
		sd.lifecycle.mu.Unlock()
		if ownsClient && cli != nil {
			sd.lifecycle.shutdownErr = cli.Close()
		}
		sd.lifecycle.mu.Lock()
		sd.lifecycle.state = etcdLifecycleStopped
		sd.lifecycle.mu.Unlock()
	})
	return sd.lifecycle.shutdownErr
}
