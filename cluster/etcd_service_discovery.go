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
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/pkg/errors"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"github.com/zeromicro/go-zero/core/hash"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	logutil "go.etcd.io/etcd/client/pkg/v3/logutil"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
)

type etcdServiceDiscovery struct {
	cli                 *clientv3.Client
	ownsClient          bool
	syncServersInterval time.Duration
	heartbeatTTL        time.Duration
	logHeartbeat        bool
	lastHeartbeatTime   time.Time
	leaseID             atomic.Int64
	// leasePublicationGate is a weighted reader/writer gate around leaseID. A
	// steady-state Flush takes one permit only to snapshot leaseID, releases it
	// before network I/O, and fences the subsequent etcd Txn with LeaseValue.
	// Grant publishes a generation under all permits; renewal also holds all
	// permits while reconciling remote registration; revoke holds all permits
	// while revoking the published generation.
	leasePublicationGate    *semaphore.Weighted
	selfRegistration        atomic.Pointer[selfRegistrationSnapshot]
	renewalSync             chan struct{}
	mapByTypeLock           sync.RWMutex
	serverMapByType         map[string]map[string]*Server
	serverMapByID           sync.Map
	etcdEndpoints           []string
	etcdUser                string
	etcdPass                string
	etcdPrefix              string
	etcdDialTimeout         time.Duration
	running                 atomic.Bool
	server                  *Server
	lastSyncUnixNano        atomic.Int64
	listenersLock           sync.RWMutex
	listeners               []SDListener
	revokeTimeout           time.Duration
	grantLeaseTimeout       time.Duration
	grantLeaseMaxRetries    int
	grantLeaseInterval      time.Duration
	shutdownDelay           time.Duration
	appDieChan              chan bool
	serverTypesBlacklist    []string
	syncServersParallelism  int
	syncFetchLock           sync.Mutex
	syncServersLock         sync.Mutex
	serverRevisions         map[string]int64
	serverRevisionFloor     int64
	watchRevision           int64
	consistentHashLock      sync.RWMutex
	consistentHash          map[string]*hash.ConsistentHash
	lifecycleLock           sync.Mutex
	lifecycleCtx            context.Context
	lifecycleCancel         context.CancelFunc
	lifecycleWG             sync.WaitGroup
	lifecycleState          etcdLifecycleState
	initDone                chan struct{}
	initErr                 error
	initWaitingCallbacks    atomic.Bool
	newEtcdClient           func(clientv3.Config) (*clientv3.Client, error)
	leaseCtx                context.Context
	leaseCancel             context.CancelFunc
	flushCtx                context.Context
	flushCancel             context.CancelFunc
	shutdownOnce            sync.Once
	shutdownErr             error
	beforeShutdownOnce      sync.Once
	callbackLock            sync.Mutex
	callbackNotEmpty        *sync.Cond
	callbackNotFull         *sync.Cond
	callbackQueue           []callbackWork
	callbackWaiters         int
	callbackStarted         bool
	callbackStopping        bool
	callbackDrained         chan struct{}
	callbackDrainOnce       sync.Once
	callbackBarrierWaiters  atomic.Int32
	leaderCallbackLock      sync.Mutex
	leaderCallbackNotEmpty  *sync.Cond
	leaderCallbackNotFull   *sync.Cond
	leaderCallbackQueue     []callbackWork
	leaderCallbackWaiters   int
	leaderCallbackStarted   bool
	leaderCallbackStopping  bool
	leaderCallbackDrained   chan struct{}
	leaderCallbackDrainOnce sync.Once

	// 选主相关
	electionEnable   bool   // 是否开启选举
	electionName     string // 竞选名,不传的话默认为election/server.Type
	resumeLeader     bool   // 若连上后发现自己是leader 是否使用原leader创建election. false的话会辞职
	reconnectBackOff time.Duration
	electionCancel   context.CancelFunc // 选举取消
	leaderLock       sync.RWMutex
	leaderID         string
	onLeaderChange   LeaderChangeListener
}

type callbackWork struct {
	callback func()
	done     chan struct{}
}

const callbackQueueLimit = 64

// The large fixed capacity models an exclusive permit set; it is not a limit on
// the number of callers the service discovery API is intended to support.
const leasePublicationPermits int64 = 1 << 30

var errEtcdCandidateLeaseUnfenced = errors.New("etcd candidate lease could not be fenced")

type etcdLifecycleState uint8

const (
	etcdLifecycleNew etcdLifecycleState = iota
	etcdLifecycleInitializing
	etcdLifecycleRunning
	etcdLifecycleStopping
	etcdLifecycleStopped
)

type leaseKeepAlive struct {
	id        clientv3.LeaseID
	ctx       context.Context
	responses <-chan *clientv3.LeaseKeepAliveResponse
	cancel    context.CancelFunc
}

type selfRegistrationSnapshot struct {
	payload  string
	revision int64
}

// NewEtcdServiceDiscovery ctor
func NewEtcdServiceDiscovery(
	config config.EtcdServiceDiscoveryConfig,
	server *Server,
	appDieChan chan bool,
	cli ...*clientv3.Client,
) (ServiceDiscovery, error) {
	if err := validateEtcdServerIdentity(server); err != nil {
		return nil, err
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	leaseCtx, leaseCancel := context.WithCancel(lifecycleCtx)
	flushCtx, flushCancel := context.WithCancel(lifecycleCtx)
	var client *clientv3.Client
	if len(cli) > 0 && cli[0] != nil {
		client = borrowEtcdClient(lifecycleCtx, cli[0])
	}
	sd := &etcdServiceDiscovery{
		server:                server,
		serverMapByType:       make(map[string]map[string]*Server),
		listeners:             make([]SDListener, 0),
		appDieChan:            appDieChan,
		cli:                   client,
		serverRevisions:       make(map[string]int64),
		consistentHash:        make(map[string]*hash.ConsistentHash),
		lifecycleCtx:          lifecycleCtx,
		lifecycleCancel:       lifecycleCancel,
		lifecycleState:        etcdLifecycleNew,
		newEtcdClient:         clientv3.New,
		leaseCtx:              leaseCtx,
		leaseCancel:           leaseCancel,
		flushCtx:              flushCtx,
		flushCancel:           flushCancel,
		leasePublicationGate:  semaphore.NewWeighted(leasePublicationPermits),
		renewalSync:           make(chan struct{}, 1),
		callbackDrained:       make(chan struct{}),
		leaderCallbackDrained: make(chan struct{}),
		resumeLeader:          true,
		reconnectBackOff:      time.Second * 2,
		electionName:          server.Type,
		electionEnable:        false,
	}
	sd.callbackNotEmpty = sync.NewCond(&sd.callbackLock)
	sd.callbackNotFull = sync.NewCond(&sd.callbackLock)
	sd.leaderCallbackNotEmpty = sync.NewCond(&sd.leaderCallbackLock)
	sd.leaderCallbackNotFull = sync.NewCond(&sd.leaderCallbackLock)
	sd.rememberSelfRegistration(server.AsJSONString(), 0)
	sd.configure(config)

	return sd, nil
}

func validateEtcdServerIdentity(server *Server) error {
	if server == nil {
		return errors.New("etcd service discovery requires a server")
	}
	if server.ID == "" || server.Type == "" || strings.Contains(server.ID, "/") || strings.Contains(server.Type, "/") {
		return fmt.Errorf("invalid etcd server identity: id=%q type=%q", server.ID, server.Type)
	}
	return nil
}

// borrowEtcdClient gives local code its own Client.Ctx and facade fields while
// retaining caller ownership of the shared transport services. Never Close it.
func borrowEtcdClient(ctx context.Context, source *clientv3.Client) *clientv3.Client {
	client := clientv3.NewCtxClient(ctx, clientv3.WithZapLogger(source.GetLogger()))
	client.KV = source.KV
	client.Lease = source.Lease
	client.Watcher = source.Watcher
	return client
}

func (sd *etcdServiceDiscovery) startLifecycleWorker(worker func()) bool {
	sd.lifecycleLock.Lock()
	defer sd.lifecycleLock.Unlock()
	// lifecycleLock is the admission gate pairing WaitGroup.Add with Shutdown's
	// transition to stopping. Once Shutdown can Wait, no new worker can Add.
	// Lifecycle workers may enqueue callbacks, but must not wait for callback
	// completion or a listener calling Shutdown would wait on its own worker.
	if (sd.lifecycleState != etcdLifecycleInitializing && sd.lifecycleState != etcdLifecycleRunning) || sd.lifecycleCtx.Err() != nil {
		return false
	}
	sd.lifecycleWG.Add(1)
	go func() {
		defer sd.lifecycleWG.Done()
		worker()
	}()
	return true
}

func (sd *etcdServiceDiscovery) enqueueCallback(callback func()) (<-chan struct{}, bool) {
	done := make(chan struct{})
	sd.callbackLock.Lock()
	// This bounds the application callback queue and applies backpressure instead
	// of dropping or coalescing topology callbacks. The etcd watch client may
	// buffer upstream responses while this queue is blocked, so listeners must
	// still return promptly.
	for len(sd.callbackQueue) >= callbackQueueLimit && !sd.callbackStopping {
		sd.callbackWaiters++
		sd.callbackNotFull.Wait()
		sd.callbackWaiters--
	}
	if sd.callbackStopping {
		sd.callbackLock.Unlock()
		return nil, false
	}
	sd.callbackQueue = append(sd.callbackQueue, callbackWork{callback: callback, done: done})
	if !sd.callbackStarted {
		sd.callbackStarted = true
		go sd.dispatchCallbacks()
	}
	sd.callbackNotEmpty.Signal()
	sd.callbackLock.Unlock()
	return done, true
}

func (sd *etcdServiceDiscovery) enqueueLeaderCallback(callback func()) (<-chan struct{}, bool) {
	done := make(chan struct{})
	sd.leaderCallbackLock.Lock()
	for len(sd.leaderCallbackQueue) >= callbackQueueLimit && !sd.leaderCallbackStopping {
		sd.leaderCallbackWaiters++
		sd.leaderCallbackNotFull.Wait()
		sd.leaderCallbackWaiters--
	}
	if sd.leaderCallbackStopping {
		sd.leaderCallbackLock.Unlock()
		return nil, false
	}
	sd.leaderCallbackQueue = append(sd.leaderCallbackQueue, callbackWork{callback: callback, done: done})
	if !sd.leaderCallbackStarted {
		sd.leaderCallbackStarted = true
		go sd.dispatchLeaderCallbacks()
	}
	sd.leaderCallbackNotEmpty.Signal()
	sd.leaderCallbackLock.Unlock()
	return done, true
}

func (sd *etcdServiceDiscovery) dispatchCallbacks() {
	// The dispatcher intentionally stays outside lifecycleWG: a listener is
	// allowed to call Shutdown, which must not wait for that same callback.
	defer sd.callbackDrainOnce.Do(func() { close(sd.callbackDrained) })
	for {
		sd.callbackLock.Lock()
		for len(sd.callbackQueue) == 0 && !sd.callbackStopping {
			sd.callbackNotEmpty.Wait()
		}
		if sd.callbackStopping && len(sd.callbackQueue) == 0 {
			sd.callbackLock.Unlock()
			return
		}
		work := sd.callbackQueue[0]
		sd.callbackQueue[0] = callbackWork{}
		sd.callbackQueue = sd.callbackQueue[1:]
		sd.callbackNotFull.Signal()
		sd.callbackLock.Unlock()
		if work.callback != nil {
			work.callback()
		}
		close(work.done)
	}
}

func (sd *etcdServiceDiscovery) dispatchLeaderCallbacks() {
	// Leader callbacks have their own lane so a slow topology listener cannot
	// leave LeaderID and IsLeader stale. Like the topology dispatcher, this
	// worker stays outside lifecycleWG so its listener may call Shutdown.
	defer sd.leaderCallbackDrainOnce.Do(func() { close(sd.leaderCallbackDrained) })
	for {
		sd.leaderCallbackLock.Lock()
		for len(sd.leaderCallbackQueue) == 0 && !sd.leaderCallbackStopping {
			sd.leaderCallbackNotEmpty.Wait()
		}
		if sd.leaderCallbackStopping && len(sd.leaderCallbackQueue) == 0 {
			sd.leaderCallbackLock.Unlock()
			return
		}
		work := sd.leaderCallbackQueue[0]
		sd.leaderCallbackQueue[0] = callbackWork{}
		sd.leaderCallbackQueue = sd.leaderCallbackQueue[1:]
		sd.leaderCallbackNotFull.Signal()
		sd.leaderCallbackLock.Unlock()
		if work.callback != nil {
			work.callback()
		}
		close(work.done)
	}
}

func (sd *etcdServiceDiscovery) stopCallbacks() {
	sd.callbackLock.Lock()
	// Close callback admission before lifecycle cancellation. Queued work is
	// drained in FIFO order rather than dropped; Shutdown does not join that
	// drain, so a callback may call Shutdown without waiting for itself.
	sd.callbackStopping = true
	started := sd.callbackStarted
	sd.callbackNotEmpty.Broadcast()
	sd.callbackNotFull.Broadcast()
	sd.callbackLock.Unlock()
	if !started {
		sd.callbackDrainOnce.Do(func() { close(sd.callbackDrained) })
	}
}

func (sd *etcdServiceDiscovery) stopLeaderCallbacks() {
	sd.leaderCallbackLock.Lock()
	sd.leaderCallbackStopping = true
	started := sd.leaderCallbackStarted
	sd.leaderCallbackNotEmpty.Broadcast()
	sd.leaderCallbackNotFull.Broadcast()
	sd.leaderCallbackLock.Unlock()
	if !started {
		sd.leaderCallbackDrainOnce.Do(func() { close(sd.leaderCallbackDrained) })
	}
}

func (sd *etcdServiceDiscovery) installElectionCancel(cancel context.CancelFunc) bool {
	sd.lifecycleLock.Lock()
	defer sd.lifecycleLock.Unlock()
	if (sd.lifecycleState != etcdLifecycleInitializing && sd.lifecycleState != etcdLifecycleRunning) || sd.lifecycleCtx.Err() != nil {
		return false
	}
	sd.electionCancel = cancel
	return true
}

func (sd *etcdServiceDiscovery) cancelElection() {
	sd.lifecycleLock.Lock()
	cancel := sd.electionCancel
	sd.lifecycleLock.Unlock()
	if cancel != nil {
		cancel()
	}
}

func waitForContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func withOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

// acquireLeasePublication takes the gate exclusively. A lease transition must
// hold it until its remote registration and local leaseID publication agree.
func (sd *etcdServiceDiscovery) acquireLeasePublication(ctx context.Context) (func(), error) {
	return sd.acquireLeasePublicationPermits(ctx, leasePublicationPermits)
}

// acquireSharedLeasePublication lets Flush calls overlap while excluding the
// short lease-generation selection/publication boundary. Remote Flush
// transactions run after releasing this permit and fence themselves in etcd.
func (sd *etcdServiceDiscovery) acquireSharedLeasePublication(ctx context.Context) (func(), error) {
	return sd.acquireLeasePublicationPermits(ctx, 1)
}

func (sd *etcdServiceDiscovery) acquireLeasePublicationPermits(ctx context.Context, permits int64) (func(), error) {
	if err := sd.leasePublicationGate.Acquire(ctx, permits); err != nil {
		return nil, err
	}
	return func() { sd.leasePublicationGate.Release(permits) }, nil
}

// GetSelfServer @implement ServiceDiscovery.GetSelfServer
//
//	@receiver sd
//	@return *Server
func (sd *etcdServiceDiscovery) GetSelfServer() *Server {
	return sd.server
}

func (sd *etcdServiceDiscovery) configure(config config.EtcdServiceDiscoveryConfig) {
	sd.etcdEndpoints = config.Endpoints
	sd.etcdUser = config.User
	sd.etcdPass = config.Pass
	sd.etcdDialTimeout = config.DialTimeout
	sd.etcdPrefix = config.Prefix
	sd.heartbeatTTL = config.Heartbeat.TTL
	sd.logHeartbeat = config.Heartbeat.Log
	sd.syncServersInterval = config.SyncServers.Interval
	sd.revokeTimeout = config.Revoke.Timeout
	sd.grantLeaseTimeout = config.GrantLease.Timeout
	sd.grantLeaseMaxRetries = config.GrantLease.MaxRetries
	sd.grantLeaseInterval = config.GrantLease.RetryInterval
	sd.shutdownDelay = config.Shutdown.Delay
	sd.serverTypesBlacklist = config.ServerTypesBlacklist
	sd.syncServersParallelism = config.SyncServers.Parallelism
	sd.electionEnable = config.Election.Enable
	if config.Election.Name != "" {
		sd.electionName = config.Election.Name
	}
	sd.electionName = "election/" + sd.electionName
}

func (sd *etcdServiceDiscovery) watchLeaseChan(ctx context.Context, leaseID clientv3.LeaseID, c <-chan *clientv3.LeaseKeepAliveResponse) {
	failedGrantLeaseAttempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case leaseKeepAliveResponse, ok := <-c:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				logger.Zap.Error("ETCD lease KeepAlive died, retrying in 10 seconds")
				if !waitForContext(ctx, 10*time.Second) {
					return
				}
			}
			if leaseKeepAliveResponse != nil {
				if sd.logHeartbeat {
					logger.Log.Debugf("sd: etcd lease %x renewed", leaseKeepAliveResponse.ID)
				}
				failedGrantLeaseAttempts = 0
				continue
			}
			logger.Log.Warn("sd: error renewing etcd lease, reconfiguring")
			for {
				err := sd.renewLease(ctx, leaseID)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					failedGrantLeaseAttempts = failedGrantLeaseAttempts + 1
					if errors.Is(err, constants.ErrEtcdGrantLeaseTimeout) {
						logger.Log.Warn("sd: timed out trying to grant etcd lease")
						sd.signalAppDie(ctx)
						return
					}
					if errors.Is(err, errEtcdCandidateLeaseUnfenced) {
						logger.Log.Warn("sd: could not fence candidate etcd lease")
						sd.signalAppDie(ctx)
						return
					}
					if failedGrantLeaseAttempts >= sd.grantLeaseMaxRetries {
						logger.Log.Warn("sd: exceeded max attempts to renew etcd lease")
						sd.signalAppDie(ctx)
						return
					}
					logger.Log.Warnf("sd: error granting etcd lease, will retry in %d seconds", uint64(sd.grantLeaseInterval.Seconds()))
					if !waitForContext(ctx, sd.grantLeaseInterval) {
						return
					}
					continue
				}
				return
			}
		}
	}
}

func (sd *etcdServiceDiscovery) signalAppDie(ctx context.Context) {
	if sd.appDieChan == nil {
		return
	}
	select {
	case sd.appDieChan <- true:
	case <-ctx.Done():
	}
}

// renewLease reestablishes connection with etcd
func (sd *etcdServiceDiscovery) renewLease(ctx context.Context, expectedLeaseID clientv3.LeaseID) error {
	attemptCtx, cancel := context.WithTimeout(ctx, sd.grantLeaseTimeout)
	defer cancel()
	logger.Zap.Info("waiting for etcd lease", zap.String("self", sd.server.ID))
	lease, err := sd.createLease(attemptCtx)
	if err != nil {
		return leaseRenewalError(ctx, attemptCtx, err)
	}
	activated := false
	defer func() {
		if !activated {
			lease.cancel()
		}
	}()

	// Renewal is the writer side of the gate: keep the old generation exclusive
	// until the remote registration is reconciled and the new leaseID is published.
	releasePublication, err := sd.acquireLeasePublication(attemptCtx)
	if err != nil {
		return leaseRenewalError(ctx, attemptCtx, err)
	}
	if clientv3.LeaseID(sd.leaseID.Load()) != expectedLeaseID {
		releasePublication()
		return nil
	}
	publishedRemotely, transitionErr := sd.transitionSelfRegistrationLease(attemptCtx, expectedLeaseID, lease.id)
	if transitionErr != nil {
		committed, reconcileErr := sd.reconcileCandidateRegistration(lease.id)
		if reconcileErr != nil {
			if errors.Is(reconcileErr, errEtcdCandidateLeaseUnfenced) {
				sd.flushCancel()
			}
			releasePublication()
			return leaseRenewalError(ctx, attemptCtx, reconcileErr)
		}
		if !committed {
			releasePublication()
			return leaseRenewalError(ctx, attemptCtx, transitionErr)
		}
	} else if !publishedRemotely {
		revokeErr := sd.revokeCandidateLease(lease.id)
		if revokeErr != nil {
			sd.flushCancel()
			revokeErr = fmt.Errorf("%w: %w", errEtcdCandidateLeaseUnfenced, revokeErr)
		}
		releasePublication()
		if revokeErr != nil {
			return leaseRenewalError(ctx, attemptCtx, revokeErr)
		}
		return fmt.Errorf(
			"etcd self registration is no longer attached to expected lease %x",
			expectedLeaseID,
		)
	}
	published, err := sd.activateLeaseLocked(lease, expectedLeaseID)
	releasePublication()
	if err != nil {
		return err
	}
	if !published {
		return nil
	}
	activated = true
	// Registration and lease publication are the fatal renewal boundary.
	// Resync requests are coalesced outside the bounded lease attempt so callback
	// backpressure cannot accumulate one lifecycle worker per renewal.
	sd.requestRenewalSync()
	return nil
}

func (sd *etcdServiceDiscovery) transitionSelfRegistrationLease(
	ctx context.Context,
	expectedLeaseID, candidateLeaseID clientv3.LeaseID,
) (bool, error) {
	key := getKey(sd.server.ID, sd.server.Type)
	response, err := sd.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.LeaseValue(key), "=", int64(expectedLeaseID))).
		Then(clientv3.OpPut(key, "", clientv3.WithIgnoreValue(), clientv3.WithLease(candidateLeaseID))).
		Else(clientv3.OpGet(key)).
		Commit()
	if err != nil {
		return false, err
	}
	if response.Succeeded {
		return true, nil
	}
	if len(response.Responses) != 1 {
		return false, errors.New("etcd lease transition returned an invalid comparison response")
	}
	existing := response.Responses[0].GetResponseRange()
	if existing == nil {
		return false, errors.New("etcd lease transition returned no comparison range")
	}
	if len(existing.Kvs) != 0 {
		return false, nil
	}

	payload := sd.currentSelfRegistrationPayload()
	response, err = sd.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, payload, clientv3.WithLease(candidateLeaseID))).
		Commit()
	if err != nil {
		return false, err
	}
	if response.Succeeded {
		sd.rememberSelfRegistration(payload, response.Header.GetRevision())
	}
	return response.Succeeded, nil
}

func (sd *etcdServiceDiscovery) rememberSelfRegistration(payload string, revision int64) {
	candidate := &selfRegistrationSnapshot{payload: payload, revision: revision}
	for {
		current := sd.selfRegistration.Load()
		if current != nil && current.revision > revision {
			return
		}
		if sd.selfRegistration.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func (sd *etcdServiceDiscovery) currentSelfRegistrationPayload() string {
	if current := sd.selfRegistration.Load(); current != nil {
		return current.payload
	}
	return sd.server.AsJSONString()
}

// reconcileCandidateRegistration resolves a transaction with an ambiguous
// client result. Observing the candidate lease proves the transaction committed.
// Otherwise revoking that lease fences a request which may still linearize.
func (sd *etcdServiceDiscovery) reconcileCandidateRegistration(candidateLeaseID clientv3.LeaseID) (bool, error) {
	getCtx, cancelGet := withOptionalTimeout(sd.lifecycleCtx, sd.etcdDialTimeout)
	response, getErr := sd.cli.Get(getCtx, getKey(sd.server.ID, sd.server.Type))
	cancelGet()
	if getErr == nil && len(response.Kvs) == 1 && clientv3.LeaseID(response.Kvs[0].Lease) == candidateLeaseID {
		sd.rememberSelfRegistration(string(response.Kvs[0].Value), response.Kvs[0].ModRevision)
		return true, nil
	}
	if revokeErr := sd.revokeCandidateLease(candidateLeaseID); revokeErr != nil {
		if getErr != nil {
			return false, fmt.Errorf(
				"%w: reconcile candidate lease: get failed: %v; revoke failed: %w",
				errEtcdCandidateLeaseUnfenced,
				getErr,
				revokeErr,
			)
		}
		return false, fmt.Errorf("%w: reconcile candidate lease: revoke failed: %w", errEtcdCandidateLeaseUnfenced, revokeErr)
	}
	if getErr != nil {
		return false, fmt.Errorf("reconcile candidate lease: %w", getErr)
	}
	return false, nil
}

func (sd *etcdServiceDiscovery) revokeCandidateLease(candidateLeaseID clientv3.LeaseID) error {
	revokeCtx, cancelRevoke := withOptionalTimeout(sd.lifecycleCtx, sd.etcdDialTimeout)
	defer cancelRevoke()
	_, err := sd.cli.Revoke(revokeCtx, candidateLeaseID)
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	return err
}

func (sd *etcdServiceDiscovery) requestRenewalSync() {
	select {
	case sd.renewalSync <- struct{}{}:
	default:
	}
}

func (sd *etcdServiceDiscovery) runRenewalSync(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sd.renewalSync:
			if err := sd.syncServers(ctx, true, false); err != nil && ctx.Err() == nil {
				logger.Zap.Error("error resyncing servers after lease renewal", zap.Error(err))
			}
		}
	}
}

func leaseRenewalError(parentCtx, attemptCtx context.Context, err error) error {
	if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil {
		return constants.ErrEtcdGrantLeaseTimeout
	}
	return err
}

func (sd *etcdServiceDiscovery) grantLease(ctx context.Context) error {
	lease, err := sd.createLease(ctx)
	if err != nil {
		return err
	}
	releasePublication, err := sd.acquireLeasePublication(ctx)
	if err != nil {
		lease.cancel()
		return err
	}
	published, err := sd.activateLeaseLocked(lease, 0)
	releasePublication()
	if err != nil {
		lease.cancel()
		return err
	}
	if !published {
		lease.cancel()
		return context.Canceled
	}
	return nil
}

func (sd *etcdServiceDiscovery) createLease(ctx context.Context) (*leaseKeepAlive, error) {
	// grab lease
	grantCtx, cancelGrant := context.WithTimeout(ctx, sd.etcdDialTimeout)
	l, err := sd.cli.Grant(grantCtx, int64(sd.heartbeatTTL.Seconds()))
	cancelGrant()
	if err != nil {
		return nil, err
	}
	logger.Zap.Debug("sd: got leaseID", zap.Int64("leaseID", int64(l.ID)), zap.String("self", sd.server.ID))
	// this will keep alive forever, when channel c is closed
	// it means we probably have to rebootstrap the lease
	keepAliveCtx, cancelKeepAlive := context.WithCancel(sd.leaseCtx)
	c, err := sd.cli.KeepAlive(keepAliveCtx, l.ID)
	if err != nil {
		cancelKeepAlive()
		return nil, err
	}
	// need to receive here as per etcd docs
	select {
	case <-ctx.Done():
		cancelKeepAlive()
		return nil, ctx.Err()
	case response, ok := <-c:
		if !ok || response == nil {
			cancelKeepAlive()
			return nil, errors.New("etcd lease keepalive closed before its first response")
		}
	}
	return &leaseKeepAlive{id: l.ID, ctx: keepAliveCtx, responses: c, cancel: cancelKeepAlive}, nil
}

// activateLeaseLocked admits the keepalive worker and publishes its lease ID as
// one local transition. The caller holds every leasePublicationGate permit
// while changing leaseID. Renewal keeps that exclusive hold across remote
// registration reconciliation; initial bootstrap attaches the key afterward.
func (sd *etcdServiceDiscovery) activateLeaseLocked(lease *leaseKeepAlive, expectedLeaseID clientv3.LeaseID) (bool, error) {
	sd.lifecycleLock.Lock()
	defer sd.lifecycleLock.Unlock()
	if (sd.lifecycleState != etcdLifecycleInitializing && sd.lifecycleState != etcdLifecycleRunning) || sd.lifecycleCtx.Err() != nil {
		err := sd.lifecycleCtx.Err()
		if err == nil {
			err = context.Canceled
		}
		return false, err
	}
	if expectedLeaseID != 0 && clientv3.LeaseID(sd.leaseID.Load()) != expectedLeaseID {
		return false, nil
	}
	sd.lifecycleWG.Add(1)
	sd.leaseID.Store(int64(lease.id))
	go func() {
		defer sd.lifecycleWG.Done()
		defer lease.cancel()
		sd.watchLeaseChan(lease.ctx, lease.id, lease.responses)
	}()
	return true, nil
}

// FlushServer2Cluster
//
//	@implement ServiceDiscovery.FlushServer2Cluster
//	@receiver sd
//	@param server
//	@return error
func (sd *etcdServiceDiscovery) FlushServer2Cluster(server *Server) error {
	if err := validateEtcdServerIdentity(server); err != nil {
		return err
	}
	return sd.flushServerWithLeaseContext(sd.flushCtx, server)
}

func (sd *etcdServiceDiscovery) flushServerWithLeaseContext(ctx context.Context, server *Server) error {
	payload := server.AsJSONString()
	var rejectedLeaseID clientv3.LeaseID
	for {
		// Snapshot only the lease generation under the shared gate. The etcd Txn
		// runs after release, so steady-state Flush calls do not serialize here.
		releasePublication, err := sd.acquireSharedLeasePublication(ctx)
		if err != nil {
			return err
		}
		leaseID := clientv3.LeaseID(sd.leaseID.Load())
		releasePublication()
		if leaseID == 0 {
			return errors.New("etcd service discovery has no published lease")
		}
		if leaseID == rejectedLeaseID {
			return fmt.Errorf(
				"etcd server %s/%s is not attached to published lease %x",
				server.Type,
				server.ID,
				leaseID,
			)
		}

		key := getKey(server.ID, server.Type)
		response, err := sd.cli.Txn(ctx).
			If(clientv3.Compare(clientv3.LeaseValue(key), "=", int64(leaseID))).
			Then(clientv3.OpPut(key, payload, clientv3.WithLease(leaseID))).
			Commit()
		if err != nil {
			return err
		}
		if response.Succeeded {
			if server.ID == sd.server.ID && server.Type == sd.server.Type {
				sd.rememberSelfRegistration(payload, response.Header.GetRevision())
			}
			return nil
		}
		rejectedLeaseID = leaseID
	}
}

func (sd *etcdServiceDiscovery) addServerIntoEtcd(server *Server) error {
	return sd.addServerIntoEtcdContext(sd.lifecycleCtx, server)
}

func (sd *etcdServiceDiscovery) addServerIntoEtcdContext(ctx context.Context, server *Server) error {
	if err := validateEtcdServerIdentity(server); err != nil {
		return err
	}
	// Bootstrap holds its shared permit through Put so the selected lease cannot
	// be renewed or revoked before the initial key is attached. Steady-state Flush
	// releases its permit before the LeaseValue-fenced transaction.
	releasePublication, err := sd.acquireSharedLeasePublication(ctx)
	if err != nil {
		return err
	}
	defer releasePublication()
	return sd.bootstrapServerWithLeaseContext(ctx, server, clientv3.LeaseID(sd.leaseID.Load()))
}

func (sd *etcdServiceDiscovery) bootstrapServer(server *Server) error {
	if err := sd.bootstrapServerContext(sd.lifecycleCtx, server); err != nil {
		return err
	}
	_ = sd.syncServers(sd.lifecycleCtx, true, true)
	return nil
}

func (sd *etcdServiceDiscovery) bootstrapServerContext(ctx context.Context, server *Server) error {
	return sd.addServerIntoEtcdContext(ctx, server)
}

func (sd *etcdServiceDiscovery) bootstrapServerWithLeaseContext(ctx context.Context, server *Server, leaseID clientv3.LeaseID) error {
	if err := validateEtcdServerIdentity(server); err != nil {
		return err
	}
	return sd.putServerValueWithLeaseContext(ctx, server.Type, server.ID, server.AsJSONString(), leaseID)
}

func (sd *etcdServiceDiscovery) putServerValueWithLeaseContext(ctx context.Context, serverType, serverID, payload string, leaseID clientv3.LeaseID) error {
	response, err := sd.cli.Put(
		ctx,
		getKey(serverID, serverType),
		payload,
		clientv3.WithLease(leaseID),
	)
	if err != nil {
		return err
	}
	if serverID == sd.server.ID && serverType == sd.server.Type {
		sd.rememberSelfRegistration(payload, response.Header.GetRevision())
	}
	return nil
}

// AddListener adds a listener to etcd service discovery
func (sd *etcdServiceDiscovery) AddListener(listener SDListener) {
	sd.listenersLock.Lock()
	defer sd.listenersLock.Unlock()
	sd.listeners = append(sd.listeners, listener)
}

// OnLeaderChange 设置leader变化监听
func (sd *etcdServiceDiscovery) OnLeaderChange(listener LeaderChangeListener) {
	sd.leaderLock.Lock()
	defer sd.leaderLock.Unlock()
	sd.onLeaderChange = listener
}

func (sd *etcdServiceDiscovery) notifyLeaderChange(leader string) {
	// Publish the leader state and invoke its listener in the same FIFO work.
	// Some listeners query IsLeader instead of trusting the callback argument,
	// so publishing a later transition early would invert Start/Stop decisions.
	_, _ = sd.enqueueLeaderCallback(func() {
		sd.leaderLock.Lock()
		sd.leaderID = leader
		listener := sd.onLeaderChange
		sd.leaderLock.Unlock()
		if listener != nil {
			listener(leader)
		}
	})
}

// AfterInit executes after Init
func (sd *etcdServiceDiscovery) AfterInit() {
}

func (sd *etcdServiceDiscovery) notifyListeners(act Action, sv *Server, old ...*Server) {
	sd.listenersLock.RLock()
	listeners := append([]SDListener(nil), sd.listeners...)
	sd.listenersLock.RUnlock()
	if len(listeners) == 0 {
		return
	}
	var oldServer *Server
	if len(old) > 0 {
		oldServer = old[0]
	}
	_, _ = sd.enqueueCallback(func() {
		for _, listener := range listeners {
			switch act {
			case DEL:
				listener.RemoveServer(sv)
			case ADD:
				listener.AddServer(sv)
			case Modify:
				listener.ModifyServer(sv, oldServer)
			}
		}
	})
}

func (sd *etcdServiceDiscovery) writeLockScope(f func()) {
	sd.mapByTypeLock.Lock()
	defer sd.mapByTypeLock.Unlock()
	f()
}

func (sd *etcdServiceDiscovery) getConsistentHash(serverType string) *hash.ConsistentHash {
	sd.consistentHashLock.RLock()
	ha := sd.consistentHash[serverType]
	sd.consistentHashLock.RUnlock()
	return ha
}

func (sd *etcdServiceDiscovery) getOrCreateConsistentHash(serverType string) *hash.ConsistentHash {
	sd.consistentHashLock.Lock()
	defer sd.consistentHashLock.Unlock()

	ha := sd.consistentHash[serverType]
	if ha == nil {
		ha = hash.NewConsistentHash()
		sd.consistentHash[serverType] = ha
	}
	return ha
}

func (sd *etcdServiceDiscovery) deleteServer(serverID string) {
	if actual, ok := sd.serverMapByID.Load(serverID); ok {
		sv := actual.(*Server)
		sd.serverMapByID.Delete(sv.ID)
		sd.writeLockScope(func() {
			if svMap, ok := sd.serverMapByType[sv.Type]; ok {
				delete(svMap, sv.ID)
			}
		})
		if ha := sd.getConsistentHash(sv.Type); ha != nil {
			ha.Remove(sv.ID)
		}
		sd.notifyListeners(DEL, sv)
	}
}

func (sd *etcdServiceDiscovery) deleteLocalInvalidServers(actualServers []string) {
	sd.serverMapByID.Range(func(key interface{}, value interface{}) bool {
		k := key.(string)
		if !util.SliceContainsString(actualServers, k) {
			logger.Sugar.Warnf("deleting invalid local server %s", k)
			sd.deleteServer(k)
		}
		return true
	})
}

// applyServerAtRevision and deleteLocalInvalidServersAtRevision are called with
// syncServersLock held. Tombstone revisions prevent a delayed watch event from
// resurrecting state that a newer snapshot already removed.
func (sd *etcdServiceDiscovery) applyServerAtRevision(server *Server, revision int64) bool {
	if revision > 0 && revision <= sd.serverRevisions[server.ID] {
		return false
	}
	if revision > 0 {
		sd.serverRevisions[server.ID] = revision
	}
	sd.addServer(server)
	return true
}

func (sd *etcdServiceDiscovery) deleteServerAtRevision(serverID string, revision int64) bool {
	if revision > 0 && revision <= sd.serverRevisions[serverID] {
		return false
	}
	if revision > 0 {
		sd.serverRevisions[serverID] = revision
	}
	sd.deleteServer(serverID)
	return true
}

func (sd *etcdServiceDiscovery) deleteLocalInvalidServersAtRevision(actualServers map[string]struct{}, snapshotRevision int64) {
	sd.serverMapByID.Range(func(key interface{}, value interface{}) bool {
		serverID := key.(string)
		if _, ok := actualServers[serverID]; ok {
			return true
		}
		if snapshotRevision > 0 && sd.serverRevisions[serverID] > snapshotRevision {
			return true
		}
		logger.Sugar.Warnf("deleting invalid local server %s", serverID)
		sd.deleteServerAtRevision(serverID, snapshotRevision)
		return true
	})
}

// pruneServerRevisionTombstones is called with syncServersLock held after a
// complete snapshot has established serverRevisionFloor. Revisions newer than
// the snapshot may belong to concurrent watch events and must survive until a
// later snapshot or complete watch response covers their authoritative state.
func (sd *etcdServiceDiscovery) pruneServerRevisionTombstones(snapshotRevision int64) {
	for serverID, revision := range sd.serverRevisions {
		if revision > snapshotRevision {
			continue
		}
		if _, loaded := sd.serverMapByID.Load(serverID); loaded {
			continue
		}
		delete(sd.serverRevisions, serverID)
	}
}

func (sd *etcdServiceDiscovery) pruneServerRevisionCandidates(serverIDs map[string]struct{}) {
	for serverID := range serverIDs {
		if _, loaded := sd.serverMapByID.Load(serverID); loaded {
			continue
		}
		if revision, ok := sd.serverRevisions[serverID]; ok && revision <= sd.serverRevisionFloor {
			delete(sd.serverRevisions, serverID)
		}
	}
}

func getKey(serverID, serverType string) string {
	return fmt.Sprintf("servers/%s/%s", serverType, serverID)
}

func getServerFromEtcd(cli *clientv3.Client, serverType, serverID string) (*Server, error) {
	return getServerFromEtcdContext(context.Background(), cli, serverType, serverID)
}

func getServerFromEtcdContext(ctx context.Context, cli *clientv3.Client, serverType, serverID string) (*Server, error) {
	server, _, err := getVersionedServerFromEtcdContext(ctx, cli, serverType, serverID, 0)
	return server, err
}

func getVersionedServerFromEtcdContext(ctx context.Context, cli *clientv3.Client, serverType, serverID string, atRevision int64) (*Server, int64, error) {
	payload, revision, err := getServerValueFromEtcdContext(ctx, cli, serverType, serverID, atRevision)
	if err != nil {
		return nil, 0, err
	}
	server, err := parseServerForKey(payload, serverType, serverID)
	if err != nil {
		return nil, 0, err
	}
	return server, revision, nil
}

func getServerValueFromEtcdContext(ctx context.Context, cli *clientv3.Client, serverType, serverID string, atRevision int64) ([]byte, int64, error) {
	svKey := getKey(serverID, serverType)
	var options []clientv3.OpOption
	if atRevision > 0 {
		options = append(options, clientv3.WithRev(atRevision))
	}
	svEInfo, err := cli.Get(ctx, svKey, options...)
	if err != nil {
		return nil, 0, fmt.Errorf("error getting server %s from etcd: %w", svKey, err)
	}
	if len(svEInfo.Kvs) == 0 {
		return nil, 0, fmt.Errorf("didn't found server: %s in etcd", svKey)
	}
	return svEInfo.Kvs[0].Value, svEInfo.Kvs[0].ModRevision, nil
}

// GetServersByType returns a slice with all the servers of a certain type
func (sd *etcdServiceDiscovery) GetServersByType(serverType string) (map[string]*Server, error) {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	if m, ok := sd.serverMapByType[serverType]; ok && len(m) > 0 {
		// Create a new map to avoid concurrent read and write access to the
		// map, this also prevents accidental changes to the list of servers
		// kept by the service discovery.
		ret := make(map[string]*Server, len(sd.serverMapByType[serverType]))
		for k, v := range sd.serverMapByType[serverType] {
			ret[k] = v
		}
		return ret, nil
	}
	return nil, constants.ErrNoServersAvailableOfType
}

func (sd *etcdServiceDiscovery) GetConsistentHashNode(serverType string, sessionID string) (string, error) {
	ha := sd.getConsistentHash(serverType)
	if ha == nil {
		return "", errors.WithStack(fmt.Errorf("%w server=%s,sid=%s", constants.ErrServerNotFound, serverType, sessionID))
	}
	node, ok := ha.Get(sessionID)
	if !ok {
		return "", errors.WithStack(fmt.Errorf("%w server=%s,sid=%s", constants.ErrServerNotFound, serverType, sessionID))
	}
	return node.(string), nil
}

// GetServers returns a slice with all the servers
func (sd *etcdServiceDiscovery) GetServers() []*Server {
	ret := make([]*Server, 0)
	sd.serverMapByID.Range(func(k, v interface{}) bool {
		ret = append(ret, v.(*Server))
		return true
	})
	return ret
}

func (sd *etcdServiceDiscovery) GetAnyFrontend() (*Server, error) {
	var frontend *Server
	sd.serverMapByID.Range(func(k, v interface{}) bool {
		sv := v.(*Server)
		if sv.Frontend {
			frontend = sv
			return false
		}
		return true
	})
	if frontend == nil {
		return nil, errors.New("not found any frontend")
	}
	return frontend, nil
}

// GetServerTypes
//
//	@implement ServiceDiscovery.GetServerTypes
func (sd *etcdServiceDiscovery) GetServerTypes() map[string]*Server {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	ret := make(map[string]*Server, len(sd.serverMapByType))
	for t, m := range sd.serverMapByType {
		for _, server := range m {
			ret[t] = server
			break
		}
	}
	return ret
}

func (sd *etcdServiceDiscovery) bootstrap() error {
	if err := sd.grantLease(sd.leaseCtx); err != nil {
		return err
	}

	// 选举
	if sd.electionEnable {
		ctx, cancel := context.WithCancel(sd.lifecycleCtx)
		if !sd.installElectionCancel(cancel) {
			cancel()
			return sd.lifecycleCtx.Err()
		}
		leaderChan, err := sd.runElection(ctx)
		if err != nil {
			cancel()
			return err
		}
		if !sd.startLifecycleWorker(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case leader, ok := <-leaderChan:
					if !ok {
						return
					}
					sd.notifyLeaderChange(leader)
				}
			}
		}) {
			cancel()
			return sd.lifecycleCtx.Err()
		}

	}

	if err := sd.bootstrapServerContext(sd.lifecycleCtx, sd.server); err != nil {
		return err
	}
	sd.syncServersLock.Lock()
	sd.applyServerAtRevision(sd.server, 0)
	sd.syncServersLock.Unlock()
	// Registration has historically been the fatal bootstrap boundary. A
	// transient initial read must not turn a registered production server into
	// a terminal Init failure; the periodic sync and watch paths will converge.
	sd.initWaitingCallbacks.Store(true)
	_ = sd.syncServers(sd.lifecycleCtx, true, true)
	sd.initWaitingCallbacks.Store(false)

	return nil
}

// GetServer returns a server given it's id
func (sd *etcdServiceDiscovery) GetServer(id string) (*Server, error) {
	if sv, ok := sd.serverMapByID.Load(id); ok {
		return sv.(*Server), nil
	}
	return nil, errors.WithStack(fmt.Errorf("%w:%s", constants.ErrNoServerWithID, id))
}

func (sd *etcdServiceDiscovery) newOwnedClient() (*clientv3.Client, error) {
	logger.Zap.Info("Initializing ETCD client")
	etcdClientLogger, _ := logutil.CreateDefaultZapLogger(logutil.ConvertToZapLevel("error"))
	config := clientv3.Config{
		Endpoints:   sd.etcdEndpoints,
		DialTimeout: sd.etcdDialTimeout,
		Logger:      etcdClientLogger,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
	}
	if sd.etcdUser != "" && sd.etcdPass != "" {
		config.Username = sd.etcdUser
		config.Password = sd.etcdPass
	}
	cli, err := sd.newEtcdClient(config)
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
	sd.lifecycleLock.Lock()
	cli := sd.cli
	initializing := sd.lifecycleState == etcdLifecycleInitializing
	sd.lifecycleLock.Unlock()
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
	sd.lifecycleLock.Lock()
	if sd.lifecycleState != etcdLifecycleInitializing {
		sd.lifecycleLock.Unlock()
		_ = candidate.Close()
		return context.Canceled
	}
	sd.cli = candidate
	sd.ownsClient = true
	sd.lifecycleLock.Unlock()
	return nil
}

// Init starts the service discovery client
func (sd *etcdServiceDiscovery) Init() error {
	sd.lifecycleLock.Lock()
	switch sd.lifecycleState {
	case etcdLifecycleNew:
		sd.lifecycleState = etcdLifecycleInitializing
		sd.initDone = make(chan struct{})
	case etcdLifecycleInitializing:
		initDone := sd.initDone
		sd.lifecycleLock.Unlock()
		<-initDone
		sd.lifecycleLock.Lock()
		err := sd.initErr
		sd.lifecycleLock.Unlock()
		return err
	case etcdLifecycleRunning:
		sd.lifecycleLock.Unlock()
		return nil
	case etcdLifecycleStopping:
		initDone := sd.initDone
		if initDone != nil {
			select {
			case <-initDone:
			default:
				sd.lifecycleLock.Unlock()
				<-initDone
				sd.lifecycleLock.Lock()
				err := sd.initErr
				sd.lifecycleLock.Unlock()
				return err
			}
		}
		fallthrough
	case etcdLifecycleStopped:
		err := sd.initErr
		if err == nil {
			err = sd.lifecycleCtx.Err()
		}
		if err == nil {
			err = context.Canceled
		}
		sd.lifecycleLock.Unlock()
		return err
	}
	sd.lifecycleLock.Unlock()

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
	if !sd.startLifecycleWorker(func() { sd.runRenewalSync(sd.lifecycleCtx) }) {
		return sd.lifecycleCtx.Err()
	}
	// Establish the initial snapshot revision before starting the watch. The
	// watcher resumes at revision+1, closing the list/watch creation window.
	if !sd.startLifecycleWorker(func() { sd.watchEtcdChanges(sd.lifecycleCtx) }) {
		return sd.lifecycleCtx.Err()
	}

	// update servers
	if !sd.startLifecycleWorker(func() {
		syncServersTicker := time.NewTicker(sd.syncServersInterval)
		defer syncServersTicker.Stop()
		for {
			select {
			case <-syncServersTicker.C:
				err := sd.syncServers(sd.lifecycleCtx, false, false)
				if err != nil && sd.lifecycleCtx.Err() == nil {
					logger.Zap.Error("error resyncing servers", zap.Error(err))
				}
			case <-sd.lifecycleCtx.Done():
				return
			}
		}
	}) {
		return sd.lifecycleCtx.Err()
	}

	return nil
}

func (sd *etcdServiceDiscovery) finishInit(initErr error) error {
	sd.lifecycleLock.Lock()
	result := initErr
	if sd.lifecycleState != etcdLifecycleInitializing && result == nil {
		result = sd.lifecycleCtx.Err()
		if result == nil {
			result = context.Canceled
		}
	}
	if result == nil {
		sd.lifecycleState = etcdLifecycleRunning
		sd.running.Store(true)
	} else {
		sd.initErr = result
		if sd.lifecycleState == etcdLifecycleInitializing {
			sd.lifecycleState = etcdLifecycleStopping
		}
	}
	close(sd.initDone)
	sd.lifecycleLock.Unlock()

	if result != nil {
		_ = sd.Shutdown()
	}
	return result
}

func parseEtcdKey(key string) (string, string, error) {
	splittedServer := strings.Split(key, "/")
	if len(splittedServer) != 3 {
		return "", "", fmt.Errorf("error parsing etcd key %s (server name can't contain /)", key)
	}
	svType := splittedServer[1]
	svID := splittedServer[2]
	return svType, svID, nil
}

func parseServer(value []byte) (*Server, error) {
	var sv *Server
	err := json.Unmarshal(value, &sv)
	if err != nil {
		logger.Zap.Warn("failed to load server", zap.Any("server", sv), zap.Error(err))
		return nil, err
	}
	if sv == nil {
		return nil, errors.New("server payload is null")
	}
	if sv.ID == "" || sv.Type == "" {
		return nil, fmt.Errorf("server payload has empty identity: id=%q type=%q", sv.ID, sv.Type)
	}
	sv.fix()
	return sv, nil
}

func parseServerForKey(value []byte, serverType, serverID string) (*Server, error) {
	sv, err := parseServer(value)
	if err != nil {
		return nil, err
	}
	if sv.ID != serverID || sv.Type != serverType {
		return nil, fmt.Errorf(
			"server payload identity %q/%q does not match etcd key %q/%q",
			sv.Type,
			sv.ID,
			serverType,
			serverID,
		)
	}
	return sv, nil
}

func (sd *etcdServiceDiscovery) printServers() {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	for k, v := range sd.serverMapByType {
		logger.Zap.Debug("", zap.String("self", sd.server.ID), zap.String("type", k), zap.Any("servers", v))
	}
}

// Struct that encapsulates a parallel/concurrent etcd get
// it spawns goroutines and receives work requests through a channel
type parallelGetterWork struct {
	serverType     string
	serverID       string
	payload        []byte
	payloadPresent bool
	revision       int64
	readAt         int64
}

type versionedServer struct {
	serverID string
	server   *Server
	revision int64
}

type parallelGetter struct {
	cli         *clientv3.Client
	ctx         context.Context
	numWorkers  int
	wg          *sync.WaitGroup
	workerWG    sync.WaitGroup
	resultMutex sync.Mutex
	result      *[]versionedServer
	firstError  error
	workChan    chan parallelGetterWork
}

func newParallelGetter(ctx context.Context, cli *clientv3.Client, numWorkers int) (*parallelGetter, error) {
	if numWorkers <= 0 {
		numWorkers = 10
	}
	p := parallelGetter{
		cli:        cli,
		ctx:        ctx,
		numWorkers: numWorkers,
		workChan:   make(chan parallelGetterWork),
		wg:         new(sync.WaitGroup),
		result:     new([]versionedServer),
	}
	if err := p.start(); err != nil {
		return nil, errors.Wrap(err, "start parallel etcd getter")
	}
	return &p, nil
}

func (p *parallelGetter) start() error {
	for i := 0; i < p.numWorkers; i++ {
		p.workerWG.Add(1)
		if err := co.Submit(func() {
			defer p.workerWG.Done()
			for work := range p.workChan {
				p.processWork(work)
			}
		}); err != nil {
			p.workerWG.Done()
			close(p.workChan)
			p.workerWG.Wait()
			return err
		}
	}
	return nil
}

func (p *parallelGetter) processWork(work parallelGetterWork) {
	defer p.wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("panic loading server %s/%s from etcd: %v", work.serverType, work.serverID, recovered)
			logger.Zap.Error("panic loading server from etcd", zap.String("svType", work.serverType), zap.String("svID", work.serverID), zap.Any("panic", recovered))
			p.recordError(err)
		}
	}()

	logger.Zap.Debug("loading info from missing server", zap.String("svType", work.serverType), zap.String("svID", work.serverID))
	payload := work.payload
	revision := work.revision
	var err error
	if !work.payloadPresent {
		payload, revision, err = getServerValueFromEtcdContext(p.ctx, p.cli, work.serverType, work.serverID, work.readAt)
	}
	if err != nil {
		logger.Zap.Error("Error loading server from etcd", zap.String("svType", work.serverType), zap.String("svID", work.serverID), zap.Error(err))
		p.recordError(err)
		return
	}

	sv, parseErr := parseServerForKey(payload, work.serverType, work.serverID)
	if parseErr != nil {
		logger.Zap.Error("Error parsing server from etcd", zap.String("svType", work.serverType), zap.String("svID", work.serverID), zap.Error(parseErr))
	}

	p.resultMutex.Lock()
	*p.result = append(*p.result, versionedServer{serverID: work.serverID, server: sv, revision: revision})
	p.resultMutex.Unlock()
}

func (p *parallelGetter) recordError(err error) {
	p.resultMutex.Lock()
	if p.firstError == nil {
		p.firstError = err
	}
	p.resultMutex.Unlock()
}

func (p *parallelGetter) waitAndGetResult() ([]versionedServer, error) {
	p.wg.Wait()
	close(p.workChan)
	p.workerWG.Wait()
	return *p.result, p.firstError
}

func (p *parallelGetter) addWorkWithPayload(serverType, serverID string, payload []byte, revision int64) {
	p.wg.Add(1)
	p.workChan <- parallelGetterWork{
		serverType:     serverType,
		serverID:       serverID,
		payload:        payload,
		payloadPresent: true,
		revision:       revision,
	}
}

func (p *parallelGetter) addWork(serverType, serverID string, readAt int64) {
	p.wg.Add(1)
	p.workChan <- parallelGetterWork{
		serverType: serverType,
		serverID:   serverID,
		readAt:     readAt,
	}
}

type etcdServerSnapshot struct {
	revision  int64
	serverIDs map[string]struct{}
	servers   []versionedServer
}

func (sd *etcdServiceDiscovery) fetchServerSnapshot(ctx context.Context, fullValues bool) (*etcdServerSnapshot, error) {
	options := []clientv3.OpOption{clientv3.WithPrefix()}
	if !fullValues {
		options = append(options, clientv3.WithKeysOnly())
	}
	kvs, err := sd.cli.Get(ctx, "servers/", options...)
	if err != nil {
		return nil, err
	}
	if kvs.Header == nil {
		return nil, errors.New("etcd server snapshot has no response header")
	}

	sd.syncServersLock.Lock()
	appliedRevisions := make(map[string]int64, len(sd.serverRevisions))
	for serverID, revision := range sd.serverRevisions {
		appliedRevisions[serverID] = revision
	}
	sd.syncServersLock.Unlock()

	snapshot := &etcdServerSnapshot{
		revision:  kvs.Header.Revision,
		serverIDs: make(map[string]struct{}, len(kvs.Kvs)),
	}
	parallelGetter, err := newParallelGetter(ctx, sd.cli, sd.syncServersParallelism)
	if err != nil {
		return nil, err
	}

	for _, kv := range kvs.Kvs {
		svType, svID, err := parseEtcdKey(string(kv.Key))
		if err != nil {
			logger.Zap.Warn("failed to parse etcd", zap.ByteString("key", kv.Key), zap.String("self", sd.server.ID), zap.Error(err))
			continue
		}
		if sd.shouldIgnoreServer(svType, svID) {
			logger.Zap.Debug("ignoring blacklisted server type", zap.String("svType", svType), zap.String("self", sd.server.ID))
			continue
		}

		snapshot.serverIDs[svID] = struct{}{}
		_, loaded := sd.serverMapByID.Load(svID)
		appliedRevision := appliedRevisions[svID]
		if appliedRevision > kv.ModRevision || (loaded && appliedRevision == kv.ModRevision) {
			continue
		}
		if fullValues {
			parallelGetter.addWorkWithPayload(svType, svID, kv.Value, kv.ModRevision)
		} else {
			// Read the payload from the keys-only snapshot revision. A current
			// read could mix a later PUT with the earlier deletion set.
			parallelGetter.addWork(svType, svID, snapshot.revision)
		}
	}

	snapshot.servers, err = parallelGetter.waitAndGetResult()
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (sd *etcdServiceDiscovery) applyServerSnapshot(snapshot *etcdServerSnapshot, waitCallbacks bool) (<-chan struct{}, bool) {
	sd.syncServersLock.Lock()
	if sd.serverRevisionFloor == 0 || snapshot.revision > sd.serverRevisionFloor {
		for _, versioned := range snapshot.servers {
			if versioned.server == nil {
				if versioned.revision > sd.serverRevisions[versioned.serverID] {
					sd.serverRevisions[versioned.serverID] = versioned.revision
				}
				continue
			}
			logger.Zap.Debug("adding server", zap.String("self", sd.server.ID), zap.Any("server", versioned.server))
			sd.applyServerAtRevision(versioned.server, versioned.revision)
		}
		sd.deleteLocalInvalidServersAtRevision(snapshot.serverIDs, snapshot.revision)
		if snapshot.revision > 0 {
			// The snapshot is authoritative through this revision. Once the floor
			// is published, old watch events no longer need per-server tombstones.
			sd.serverRevisionFloor = snapshot.revision
		}
		sd.pruneServerRevisionTombstones(snapshot.revision)
		if snapshot.revision > sd.watchRevision {
			// Full snapshots carry every payload. Keys-only snapshots reach this
			// point only after every missing or lagging payload was fetched at the
			// same revision, so both forms may safely advance the checkpoint.
			sd.watchRevision = snapshot.revision
		}
	}
	sd.lastSyncUnixNano.Store(time.Now().UnixNano())
	var callbackBarrier <-chan struct{}
	callbackAccepted := true
	if waitCallbacks {
		// Public SyncServers keeps its historical completion boundary: all
		// listener work admitted before this marker finishes before return. A
		// callback running elsewhere is never a reason to skip this marker.
		callbackBarrier, callbackAccepted = sd.enqueueCallback(nil)
	}
	sd.syncServersLock.Unlock()
	return callbackBarrier, callbackAccepted
}

// SyncServers gets all servers from etcd
func (sd *etcdServiceDiscovery) SyncServers(firstSync bool) error {
	return sd.syncServers(sd.lifecycleCtx, firstSync, true)
}

func (sd *etcdServiceDiscovery) syncServers(ctx context.Context, firstSync, waitCallbacks bool) error {
	sd.syncFetchLock.Lock()
	snapshot, err := sd.fetchServerSnapshot(ctx, firstSync)
	if err != nil && !firstSync && errors.Is(err, rpctypes.ErrCompacted) {
		// A keys-only snapshot can be compacted before its historical payload
		// reads complete. Fall back to one current full-value snapshot.
		snapshot, err = sd.fetchServerSnapshot(ctx, true)
	}
	if err != nil {
		sd.syncFetchLock.Unlock()
		if ctx.Err() == nil {
			logger.Zap.Error("Error querying etcd server", zap.Error(err))
		}
		return err
	}
	callbackBarrier, callbackAccepted := sd.applyServerSnapshot(snapshot, waitCallbacks)
	sd.syncFetchLock.Unlock()
	if !callbackAccepted {
		sd.callbackBarrierWaiters.Add(1)
		defer sd.callbackBarrierWaiters.Add(-1)
		select {
		case <-sd.callbackDrained:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if callbackBarrier != nil {
		select {
		case <-callbackBarrier:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (sd *etcdServiceDiscovery) Running() bool {
	return sd.running.Load()
}

// BeforeShutdown executes before shutting down and will remove the server from the list
func (sd *etcdServiceDiscovery) BeforeShutdown() {
	sd.lifecycleLock.Lock()
	lifecycleState := sd.lifecycleState
	sd.lifecycleLock.Unlock()
	if lifecycleState < etcdLifecycleRunning {
		return
	}
	sd.beforeShutdownOnce.Do(func() {
		sd.stopCallbacks()
		sd.stopLeaderCallbacks()
		// A listener may be blocked in FlushServer2Cluster. Cancel Flush before
		// joining either callback lane or the drain and revoke phases could wait on
		// each other indefinitely.
		sd.flushCancel()
		// BeforeShutdown is the non-reentrant lifecycle barrier: dependent modules
		// must not begin teardown while already accepted listeners in either lane
		// can still run. Shutdown intentionally does not join so a listener may
		// call it itself.
		<-sd.callbackDrained
		<-sd.leaderCallbackDrained
		sd.listenersLock.Lock()
		sd.listeners = nil
		sd.listenersLock.Unlock()
		sd.leaderLock.Lock()
		sd.onLeaderChange = nil
		sd.leaderLock.Unlock()
		if !sd.Running() {
			// Shutdown is callback-safe because it does not join these drains, but it
			// also does not revoke. If it ran first, leave the registration to lease
			// expiry; normal module teardown must call BeforeShutdown first.
			return
		}
		// Flush transactions compare the lease generation, so an RPC which
		// linearizes after cancellation cannot recreate the revoked registration.
		sd.leaseCancel()
		_ = sd.revoke()
		sd.cancelElection()
		time.Sleep(sd.shutdownDelay) // Sleep for a short while to ensure shutdown has propagated
	})
}

// Shutdown executes on shutdown and will clean etcd
func (sd *etcdServiceDiscovery) Shutdown() error {
	sd.shutdownOnce.Do(func() {
		sd.running.Store(false)
		sd.lifecycleLock.Lock()
		initializing := sd.lifecycleState == etcdLifecycleInitializing
		initWaitingCallbacks := sd.initWaitingCallbacks.Load()
		if sd.lifecycleState < etcdLifecycleStopping {
			sd.lifecycleState = etcdLifecycleStopping
		}
		initDone := sd.initDone
		electionCancel := sd.electionCancel
		sd.lifecycleLock.Unlock()

		// Stop worker and both callback admissions before cancellation and Wait.
		// This ordering is the shutdown gate documented above each subsystem.
		sd.stopCallbacks()
		sd.stopLeaderCallbacks()
		sd.flushCancel()
		sd.leaseCancel()
		sd.lifecycleCancel()
		if electionCancel != nil {
			electionCancel()
		}
		// Init may be waiting for an initial listener which calls Shutdown. Only
		// that reentrant window skips the join; other initialization races retain
		// Shutdown's historical completion boundary.
		if initDone != nil && (!initializing || !initWaitingCallbacks) {
			<-initDone
		}

		sd.lifecycleWG.Wait()
		sd.lifecycleLock.Lock()
		cli := sd.cli
		ownsClient := sd.ownsClient
		sd.lifecycleLock.Unlock()
		if ownsClient && cli != nil {
			sd.shutdownErr = cli.Close()
		}
		sd.lifecycleLock.Lock()
		sd.lifecycleState = etcdLifecycleStopped
		sd.lifecycleLock.Unlock()
	})
	return sd.shutdownErr
}

// revoke prevents Pitaya from crashing when etcd is not available
func (sd *etcdServiceDiscovery) revoke() error {
	ctx, cancel := context.WithTimeout(context.Background(), sd.revokeTimeout)
	defer cancel()
	logger.Zap.Debug("waiting for etcd revoke", zap.String("self", sd.server.ID))
	// Exclude lease selection and publication while revoking. A Flush transaction
	// that already released its shared permit remains safe: its LeaseValue compare
	// either commits before revoke or fails after the leased key is removed.
	releasePublication, err := sd.acquireLeasePublication(ctx)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Zap.Warn("timed out waiting for etcd revoke", zap.String("self", sd.server.ID))
			return nil
		}
		return err
	}
	defer releasePublication()
	_, err = sd.cli.Revoke(ctx, clientv3.LeaseID(sd.leaseID.Load()))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		logger.Zap.Warn("timed out waiting for etcd revoke", zap.String("self", sd.server.ID))
		return nil // timed out
	}
	logger.Zap.Debug("finished waiting for etcd revoke", zap.String("self", sd.server.ID))
	return err
}

func (sd *etcdServiceDiscovery) addServer(sv *Server) {
	old, loaded := sd.serverMapByID.Load(sv.ID)
	sd.writeLockScope(func() {
		sd.serverMapByID.Store(sv.ID, sv)
		mapSvByType, ok := sd.serverMapByType[sv.Type]
		if !ok {
			mapSvByType = make(map[string]*Server)
			sd.serverMapByType[sv.Type] = mapSvByType
		}
		mapSvByType[sv.ID] = sv
	})
	if sv.ID != sd.server.ID {
		if !loaded {
			sd.getOrCreateConsistentHash(sv.Type).Add(sv.ID)
			sd.notifyListeners(ADD, sv)
		} else {
			sd.notifyListeners(Modify, sv, old.(*Server))
		}
	}
}

func (sd *etcdServiceDiscovery) watchEtcdChanges(ctx context.Context) {
	for {
		watchCtx, cancelWatch := context.WithCancel(ctx)
		sd.syncServersLock.Lock()
		watchRevision := sd.watchRevision
		sd.syncServersLock.Unlock()
		watchOptions := []clientv3.OpOption{
			clientv3.WithPrefix(),
			clientv3.WithCreatedNotify(),
			clientv3.WithProgressNotify(),
		}
		if watchRevision > 0 {
			watchOptions = append(watchOptions, clientv3.WithRev(watchRevision+1))
		}
		watchChan := sd.cli.Watch(watchCtx, "servers/", watchOptions...)
		retryDelay := time.Second
		for watchChan != nil {
			select {
			case <-ctx.Done():
				cancelWatch()
				return
			case watchResponse, ok := <-watchChan:
				if !ok {
					if ctx.Err() != nil {
						cancelWatch()
						return
					}
					logger.Zap.Error("etcd watcher died, retrying to watch in 1 second", zap.String("self", sd.server.ID))
					watchChan = nil
					continue
				}
				if err := watchResponse.Err(); err != nil {
					logger.Zap.Warn("etcd watcher response error", zap.String("self", sd.server.ID), zap.Error(err))
					retryDelay = 100 * time.Millisecond
					if watchResponse.CompactRevision != 0 || errors.Is(err, rpctypes.ErrCompacted) {
						// A compacted checkpoint cannot be skipped. A complete current
						// snapshot establishes both state and the next watch revision.
						if syncErr := sd.syncServers(ctx, true, false); syncErr != nil && ctx.Err() == nil {
							logger.Zap.Error("failed to recover compacted etcd watch", zap.String("self", sd.server.ID), zap.Error(syncErr))
						}
					}
					watchChan = nil
					continue
				}
				if watchResponse.Created && len(watchResponse.Events) == 0 {
					if watchRevision == 0 {
						// A watch created without a base revision starts after its
						// creation point and cannot replay keys that already exist. Build
						// an authoritative snapshot, then restart at snapshot revision+1.
						if syncErr := sd.syncServers(ctx, true, false); syncErr != nil && ctx.Err() == nil {
							logger.Zap.Error("failed to establish initial etcd watch snapshot", zap.String("self", sd.server.ID), zap.Error(syncErr))
						}
						watchChan = nil
						continue
					}
					// A watch without WithRev starts after the creation revision, so
					// that revision is a safe reconnect checkpoint. A historical watch
					// may still have backlog after Created and must not jump to Header.
					sd.recordWatchCreation(watchRevision, watchResponse.Header.Revision)
					continue
				}
				if !sd.applyEtcdWatchResponse(watchResponse) {
					logger.Zap.Error("invalid etcd watch response; recovering from snapshot", zap.String("self", sd.server.ID))
					if syncErr := sd.syncServers(ctx, true, false); syncErr != nil && ctx.Err() == nil {
						logger.Zap.Error("failed to recover invalid etcd watch response", zap.String("self", sd.server.ID), zap.Error(syncErr))
					}
					watchChan = nil
				}
			}
		}
		cancelWatch()
		if !waitForContext(ctx, retryDelay) {
			return
		}
	}
}

func (sd *etcdServiceDiscovery) recordWatchCreation(requestedRevision, createdRevision int64) {
	if requestedRevision != 0 || createdRevision <= 0 {
		return
	}
	sd.syncServersLock.Lock()
	if sd.watchRevision == 0 {
		sd.watchRevision = createdRevision
	}
	sd.syncServersLock.Unlock()
}

// applyEtcdWatchResponse returns false when the response cannot form a
// contiguous checkpoint. The caller must replace that watch with a complete
// snapshot before accepting later responses.
func (sd *etcdServiceDiscovery) applyEtcdWatchResponse(watchResponse clientv3.WatchResponse) bool {
	if watchResponse.Created {
		return len(watchResponse.Events) == 0
	}
	if len(watchResponse.Events) == 0 {
		sd.syncServersLock.Lock()
		if watchResponse.Header.Revision > sd.watchRevision {
			sd.watchRevision = watchResponse.Header.Revision
		}
		if sd.serverRevisionFloor > 0 && watchResponse.Header.Revision > sd.serverRevisionFloor {
			sd.serverRevisionFloor = watchResponse.Header.Revision
			sd.pruneServerRevisionTombstones(watchResponse.Header.Revision)
		}
		sd.syncServersLock.Unlock()
		return true
	}

	var responseRevision int64
	for _, ev := range watchResponse.Events {
		if ev == nil || ev.Kv == nil || ev.Kv.ModRevision <= 0 {
			return false
		}
		switch ev.Type {
		case clientv3.EventTypePut, clientv3.EventTypeDelete:
		default:
			return false
		}
		if ev.Kv.ModRevision > responseRevision {
			responseRevision = ev.Kv.ModRevision
		}
	}

	sd.syncServersLock.Lock()
	defer sd.syncServersLock.Unlock()
	pruneCandidates := make(map[string]struct{}, len(watchResponse.Events))
	for _, ev := range watchResponse.Events {
		revision := ev.Kv.ModRevision
		if revision <= sd.serverRevisionFloor {
			continue
		}
		svType, svID, err := parseEtcdKey(string(ev.Kv.Key))
		if err != nil {
			logger.Log.Warnf("failed to parse key from etcd: %s", ev.Kv.Key)
			continue
		}

		if sd.shouldIgnoreServer(svType, svID) {
			continue
		}
		pruneCandidates[svID] = struct{}{}

		switch ev.Type {
		case clientv3.EventTypePut:
			sv, err := parseServerForKey(ev.Kv.Value, svType, svID)
			if err != nil {
				logger.Zap.Error("Failed to parse server from etcd", zap.String("self", sd.server.ID), zap.Error(err))
				if revision > sd.serverRevisions[svID] {
					// Consume the malformed remote revision while preserving the
					// last valid local value. A later valid PUT can still recover it.
					sd.serverRevisions[svID] = revision
				}
				continue
			}

			if sd.applyServerAtRevision(sv, revision) {
				logger.Zap.Info("server added", zap.ByteString("sv", ev.Kv.Key), zap.String("self", sd.server.ID))
				sd.printServers()
			}
		case clientv3.EventTypeDelete:
			if sd.deleteServerAtRevision(svID, revision) {
				logger.Zap.Info("server deleted", zap.ByteString("sv", ev.Kv.Key), zap.String("self", sd.server.ID))
				sd.printServers()
			}
		}
	}
	if responseRevision > sd.watchRevision {
		sd.watchRevision = responseRevision
	}
	if sd.serverRevisionFloor > 0 && responseRevision > sd.serverRevisionFloor {
		sd.serverRevisionFloor = responseRevision
		sd.pruneServerRevisionCandidates(pruneCandidates)
	}
	return true
}

func (sd *etcdServiceDiscovery) isServerTypeBlacklisted(svType string) bool {
	for _, blacklistedSv := range sd.serverTypesBlacklist {
		if blacklistedSv == svType {
			return true
		}
	}
	return false
}

func (sd *etcdServiceDiscovery) shouldIgnoreServer(serverType, serverID string) bool {
	if !sd.isServerTypeBlacklisted(serverType) {
		return false
	}
	return serverType != sd.server.Type || serverID != sd.server.ID
}

type electionGeneration struct {
	ctx      context.Context
	cancel   context.CancelFunc
	session  *concurrency.Session
	election *concurrency.Election
}

type currentElection struct {
	lock     sync.RWMutex
	election *concurrency.Election
}

func (state *currentElection) load() *concurrency.Election {
	state.lock.RLock()
	defer state.lock.RUnlock()
	return state.election
}

func (state *currentElection) store(election *concurrency.Election) {
	state.lock.Lock()
	state.election = election
	state.lock.Unlock()
}

func (sd *etcdServiceDiscovery) watchLeader(
	ctx context.Context,
	state *currentElection,
	setLeader func(string),
) {
	for {
		watchCtx, cancelWatch := context.WithCancel(ctx)
		watchChan := sd.cli.Watch(watchCtx, sd.electionName+"/", clientv3.WithPrefix())
		retryDelay := time.Second
		for watchChan != nil {
			select {
			case <-ctx.Done():
				cancelWatch()
				return
			case watchResponse, ok := <-watchChan:
				if !ok {
					if ctx.Err() != nil {
						cancelWatch()
						return
					}
					logger.Log.Error("etcd election watcher died, retrying to watch in 1 second")
					watchChan = nil
					continue
				}
				if err := watchResponse.Err(); err != nil {
					logger.Zap.Warn("etcd election watcher response error", zap.String("self", sd.server.ID), zap.Error(err))
					retryDelay = 100 * time.Millisecond
					watchChan = nil
					continue
				}
				election := state.load()
				if election == nil {
					continue
				}
				resp, err := election.Leader(watchCtx)
				if errors.Is(err, concurrency.ErrElectionNoLeader) {
					setLeader("")
					continue
				}
				if err != nil {
					if ctx.Err() != nil {
						cancelWatch()
						return
					}
					logger.Log.Error("etcd election error", zap.String("self", sd.server.ID), zap.Error(err))
					continue
				}
				logger.Zap.Debug("etcd election watcher", zap.String("self", sd.server.ID), zap.ByteString("leader", resp.Kvs[0].Value))
				setLeader(string(resp.Kvs[0].Value))
			}
		}
		cancelWatch()
		if !waitForContext(ctx, retryDelay) {
			return
		}
	}
}

func (sd *etcdServiceDiscovery) runElection(ctx context.Context) (<-chan string, error) {
	var leaderID string
	var leaderIDLock sync.Mutex
	var leaderReadyOnce sync.Once
	leaderReady := make(chan struct{})
	leaderChan := make(chan string, 10)
	setLeader := func(id string) {
		leaderIDLock.Lock()
		defer leaderIDLock.Unlock()
		if leaderID == id {
			logger.Zap.Debug("etcd election leader remains the same,ignore", zap.String("leaderID", leaderID), zap.String("self", sd.server.ID))
			return
		}
		logger.Zap.Info("etcd election leader changed", zap.String("name", sd.electionName),
			zap.String("leaderID", id), zap.String("old", leaderID), zap.String("self", sd.server.ID))
		leaderID = id
		select {
		case leaderChan <- id:
			if id != "" {
				leaderReadyOnce.Do(func() { close(leaderReady) })
			}
		case <-ctx.Done():
		}
	}
	isLeader := func() bool {
		leaderIDLock.Lock()
		defer leaderIDLock.Unlock()
		return leaderID == sd.server.ID
	}

	generation, err := sd.newElectionGeneration(ctx, clientv3.LeaseID(sd.leaseID.Load()))
	if err != nil {
		return nil, errors.Wrap(err, "while creating initial session")
	}
	state := &currentElection{}
	state.store(generation.election)

	if !sd.startLifecycleWorker(func() {
		for {
			nextLease, reconnect := sd.runElectionGeneration(ctx, generation, state, setLeader, isLeader)
			state.store(nil)
			sd.stopElectionGeneration(generation)
			if !reconnect || ctx.Err() != nil {
				return
			}
			logger.Zap.Debug("etcd election need reconnect", zap.String("self", sd.server.ID))
			setLeader("")
			if nextLease == 0 && !waitForContext(ctx, sd.reconnectBackOff) {
				return
			}
			for {
				generation, err = sd.newElectionGeneration(ctx, nextLease)
				if err == nil {
					state.store(generation.election)
					break
				}
				if ctx.Err() != nil {
					return
				}
				logger.Zap.Error("while creating new session", zap.String("self", sd.server.ID), zap.Error(err))
				nextLease = 0
				if !waitForContext(ctx, sd.reconnectBackOff) {
					return
				}
			}
		}
	}) {
		sd.stopElectionGeneration(generation)
		return nil, ctx.Err()
	}

	if !sd.startLifecycleWorker(func() { sd.watchLeader(ctx, state, setLeader) }) {
		return nil, ctx.Err()
	}

	select {
	case <-leaderReady:
		return leaderChan, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (sd *etcdServiceDiscovery) runElectionGeneration(
	ctx context.Context,
	generation *electionGeneration,
	state *currentElection,
	setLeader func(string),
	isLeader func() bool,
) (clientv3.LeaseID, bool) {
	for {
		node, err := generation.election.Leader(generation.ctx)
		resumeObservation := false
		if err != nil && !errors.Is(err, concurrency.ErrElectionNoLeader) {
			if ctx.Err() == nil {
				logger.Zap.Error("while determining election leader", zap.String("self", sd.server.ID), zap.Error(err))
			}
			return 0, ctx.Err() == nil
		}
		if err == nil && len(node.Kvs) > 0 && string(node.Kvs[0].Value) == sd.server.ID {
			leaderLease := clientv3.LeaseID(node.Kvs[0].Lease)
			if generation.session.Lease() != leaderLease {
				return leaderLease, true
			}
			resumed := concurrency.ResumeElection(generation.session, sd.electionName+"/",
				string(node.Kvs[0].Key), node.Kvs[0].CreateRevision)
			generation.election = resumed
			state.store(resumed)
			if sd.resumeLeader {
				resumeObservation = true
			} else {
				resignCtx, cancel := context.WithTimeout(generation.ctx, sd.electionShutdownTimeout())
				err = resumed.Resign(resignCtx)
				cancel()
				if err != nil {
					logger.Zap.Error("while resigning leadership after reconnect", zap.String("self", sd.server.ID), zap.Error(err))
					return 0, ctx.Err() == nil
				}
				setLeader("")
			}
		}
		if !resumeObservation {
			if err != nil {
				setLeader("")
			} else if len(node.Kvs) > 0 {
				setLeader(string(node.Kvs[0].Value))
			}

			campaignCtx, cancelCampaign := context.WithCancel(generation.ctx)
			campaignDone := make(chan error, 1)
			election := generation.election
			go func() {
				campaignDone <- election.Campaign(campaignCtx, sd.server.ID)
			}()
			select {
			case err = <-campaignDone:
				cancelCampaign()
				if err != nil {
					if ctx.Err() == nil {
						logger.Zap.Error("while campaigning for leader", zap.String("self", sd.server.ID), zap.Error(err))
					}
					return 0, ctx.Err() == nil
				}
			case <-ctx.Done():
				generation.cancel()
				cancelCampaign()
				<-campaignDone
				return 0, false
			case <-generation.session.Done():
				generation.cancel()
				cancelCampaign()
				<-campaignDone
				return 0, ctx.Err() == nil
			}
		}

		setLeader(sd.server.ID)
		observeCtx, cancelObserve := context.WithCancel(generation.ctx)
		observe := generation.election.Observe(observeCtx)
		for {
			select {
			case resp, ok := <-observe:
				if !ok {
					cancelObserve()
					if ctx.Err() == nil {
						logger.Zap.Error("election observe stream closed", zap.String("self", sd.server.ID))
					}
					return 0, ctx.Err() == nil
				}
				if len(resp.Kvs) == 0 {
					continue
				}
				leader := string(resp.Kvs[0].Value)
				logger.Zap.Debug("etcd election observe", zap.String("self", sd.server.ID), zap.String("leader", leader))
				setLeader(leader)
				if leader != sd.server.ID {
					cancelObserve()
					for range observe {
					}
					break
				}
			case <-ctx.Done():
				cancelObserve()
				for range observe {
				}
				if isLeader() {
					resignCtx, cancel := context.WithTimeout(context.Background(), sd.electionShutdownTimeout())
					if err := generation.election.Resign(resignCtx); err != nil {
						logger.Zap.Error("while resigning leadership during shutdown", zap.String("self", sd.server.ID), zap.Error(err))
					}
					cancel()
				}
				return 0, false
			case <-generation.session.Done():
				cancelObserve()
				for range observe {
				}
				return 0, ctx.Err() == nil
			}
		}
	}
}

func (sd *etcdServiceDiscovery) newElectionGeneration(ctx context.Context, id clientv3.LeaseID) (*electionGeneration, error) {
	generationCtx, cancel := context.WithCancel(ctx)
	generationClient := borrowEtcdClient(generationCtx, sd.cli)
	session, err := concurrency.NewSession(generationClient, concurrency.WithTTL(int(sd.heartbeatTTL.Seconds())),
		concurrency.WithContext(generationCtx), concurrency.WithLease(id))
	if err != nil {
		cancel()
		return nil, err
	}
	return &electionGeneration{
		ctx:      generationCtx,
		cancel:   cancel,
		session:  session,
		election: concurrency.NewElection(session, sd.electionName),
	}, nil
}

func (sd *etcdServiceDiscovery) stopElectionGeneration(generation *electionGeneration) {
	generation.cancel()
	generation.session.Orphan()
}

func (sd *etcdServiceDiscovery) electionShutdownTimeout() time.Duration {
	if sd.revokeTimeout > 0 && sd.revokeTimeout < sd.heartbeatTTL {
		return sd.revokeTimeout
	}
	return sd.heartbeatTTL
}

func (sd *etcdServiceDiscovery) LeaderID() string {
	sd.leaderLock.RLock()
	defer sd.leaderLock.RUnlock()
	return sd.leaderID
}
func (sd *etcdServiceDiscovery) IsLeader() bool {
	return sd.LeaderID() == sd.server.ID
}
