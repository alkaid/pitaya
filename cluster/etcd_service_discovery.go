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
	lifecycle etcdLifecycleOwner
	lease     etcdLeaseOwner
	watch     etcdWatchOwner
	election  etcdElectionOwner

	cli             *clientv3.Client
	ownsClient      bool
	etcdEndpoints   []string
	etcdUser        string
	etcdPass        string
	etcdTLS         config.TLSConfig
	etcdPrefix      string
	etcdDialTimeout time.Duration
	server          *Server
	shutdownDelay   time.Duration
	newEtcdClient   func(clientv3.Config) (*clientv3.Client, error)
}

// The large fixed capacity models an exclusive permit set; it is not a limit on
// the number of callers the service discovery API is intended to support.
const leasePublicationPermits int64 = 1 << 30

var errEtcdCandidateLeaseUnfenced = errors.New("etcd candidate lease could not be fenced")
var errEtcdServerIdentityOwned = errors.New("etcd server identity is already owned by another lease")

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
	// RPC modules initialize before service discovery. Fence the shared server as
	// soon as etcd ownership is selected so ingress cannot accept epoch-0 calls
	// before registration publishes a real owner generation.
	server.InvalidateOwnerEpoch()
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	leaseCtx, leaseCancel := context.WithCancel(lifecycleCtx)
	flushCtx, flushCancel := context.WithCancel(lifecycleCtx)
	var client *clientv3.Client
	if len(cli) > 0 && cli[0] != nil {
		client = borrowEtcdClient(lifecycleCtx, cli[0])
	}
	sd := &etcdServiceDiscovery{
		server:        server,
		cli:           client,
		newEtcdClient: clientv3.New,
		lifecycle: etcdLifecycleOwner{
			ctx:        lifecycleCtx,
			cancel:     lifecycleCancel,
			state:      etcdLifecycleNew,
			appDieChan: appDieChan,
		},
		lease: etcdLeaseOwner{
			ctx:             leaseCtx,
			cancel:          leaseCancel,
			flushCtx:        flushCtx,
			flushCancel:     flushCancel,
			publicationGate: semaphore.NewWeighted(leasePublicationPermits),
			renewal:         make(chan struct{}, 1),
		},
		watch: etcdWatchOwner{
			byType:         make(map[string]map[string]*Server),
			listeners:      make([]SDListener, 0),
			revisions:      make(map[string]int64),
			consistentHash: make(map[string]*hash.ConsistentHash),
			callbacks:      newCallbackDispatcher(callbackQueueLimit),
		},
		election: etcdElectionOwner{
			callbacks:      newCallbackDispatcher(callbackQueueLimit),
			resumeLeader:   true,
			reconnectDelay: 2 * time.Second,
			name:           server.Type,
		},
	}
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

func (sd *etcdServiceDiscovery) installElectionCancel(cancel context.CancelFunc) bool {
	sd.lifecycle.mu.Lock()
	defer sd.lifecycle.mu.Unlock()
	if (sd.lifecycle.state != etcdLifecycleInitializing && sd.lifecycle.state != etcdLifecycleRunning) || sd.lifecycle.ctx.Err() != nil {
		return false
	}
	sd.election.cancelMu.Lock()
	sd.election.cancel = cancel
	sd.election.cancelMu.Unlock()
	return true
}

func (sd *etcdServiceDiscovery) publishSelfOwnerEpoch(epoch uint64) bool {
	sd.lifecycle.mu.Lock()
	defer sd.lifecycle.mu.Unlock()
	if (sd.lifecycle.state != etcdLifecycleInitializing && sd.lifecycle.state != etcdLifecycleRunning) ||
		(sd.lifecycle.ctx != nil && sd.lifecycle.ctx.Err() != nil) {
		return false
	}
	sd.server.SetOwnerEpoch(epoch)
	return true
}

func (sd *etcdServiceDiscovery) cancelElection() {
	sd.election.cancelMu.Lock()
	cancel := sd.election.cancel
	sd.election.cancelMu.Unlock()
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
	if err := sd.lease.publicationGate.Acquire(ctx, permits); err != nil {
		return nil, err
	}
	return func() { sd.lease.publicationGate.Release(permits) }, nil
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
	sd.etcdTLS = config.TLS
	sd.etcdDialTimeout = config.DialTimeout
	sd.etcdPrefix = config.Prefix
	sd.lease.heartbeatTTL = config.Heartbeat.TTL
	sd.lease.logHeartbeat = config.Heartbeat.Log
	sd.watch.syncInterval = config.SyncServers.Interval
	sd.lease.revokeTimeout = config.Revoke.Timeout
	sd.lease.grantTimeout = config.GrantLease.Timeout
	sd.lease.grantMaxRetries = config.GrantLease.MaxRetries
	sd.lease.grantRetryInterval = config.GrantLease.RetryInterval
	sd.shutdownDelay = config.Shutdown.Delay
	sd.watch.blacklist = config.ServerTypesBlacklist
	sd.watch.parallelism = config.SyncServers.Parallelism
	sd.election.enabled = config.Election.Enable
	if config.Election.Name != "" {
		sd.election.name = config.Election.Name
	}
	sd.election.name = "election/" + sd.election.name
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
				sd.server.InvalidateOwnerEpoch()
				logger.Zap.Error("ETCD lease KeepAlive died, retrying in 10 seconds")
				if !waitForContext(ctx, 10*time.Second) {
					return
				}
			}
			if leaseKeepAliveResponse != nil {
				if sd.lease.logHeartbeat {
					logger.Log.Debugf("sd: etcd lease %x renewed", leaseKeepAliveResponse.ID)
				}
				failedGrantLeaseAttempts = 0
				continue
			}
			sd.server.InvalidateOwnerEpoch()
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
					if failedGrantLeaseAttempts >= sd.lease.grantMaxRetries {
						logger.Log.Warn("sd: exceeded max attempts to renew etcd lease")
						sd.signalAppDie(ctx)
						return
					}
					logger.Log.Warnf("sd: error granting etcd lease, will retry in %d seconds", uint64(sd.lease.grantRetryInterval.Seconds()))
					if !waitForContext(ctx, sd.lease.grantRetryInterval) {
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
	if ctx.Err() == nil {
		signalAppDie(sd.lifecycle.appDieChan)
	}
}

// renewLease reestablishes connection with etcd
func (sd *etcdServiceDiscovery) renewLease(ctx context.Context, expectedLeaseID clientv3.LeaseID) error {
	attemptCtx, cancel := context.WithTimeout(ctx, sd.lease.grantTimeout)
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
	if clientv3.LeaseID(sd.lease.id.Load()) != expectedLeaseID {
		releasePublication()
		return nil
	}
	publishedRemotely, transitionErr := sd.transitionSelfRegistrationLease(attemptCtx, expectedLeaseID, lease.id)
	if transitionErr != nil {
		committed, reconcileErr := sd.reconcileCandidateRegistration(lease.id)
		if reconcileErr != nil {
			if errors.Is(reconcileErr, errEtcdCandidateLeaseUnfenced) {
				sd.lease.flushCancel()
			}
			releasePublication()
			return leaseRenewalError(ctx, attemptCtx, reconcileErr)
		}
		if !committed {
			releasePublication()
			return leaseRenewalError(ctx, attemptCtx, transitionErr)
		}
		publishedRemotely = true
	} else if !publishedRemotely {
		revokeErr := sd.revokeCandidateLease(lease.id)
		if revokeErr != nil {
			sd.lease.flushCancel()
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
	if err != nil || !published {
		var revokeErr error
		if publishedRemotely {
			// Keep the publication gate exclusive until the remotely committed but
			// locally untracked generation is fenced. Shutdown otherwise sees the old
			// leaseID and cannot revoke this candidate.
			revokeErr = sd.revokeCandidateLease(lease.id)
		}
		releasePublication()
		if revokeErr != nil {
			sd.lease.flushCancel()
			if err != nil {
				return fmt.Errorf(
					"%w: candidate lease %x activation failed: %v; revoke failed: %w",
					errEtcdCandidateLeaseUnfenced,
					lease.id,
					err,
					revokeErr,
				)
			}
			return fmt.Errorf(
				"%w: candidate lease %x was not activated; revoke failed: %w",
				errEtcdCandidateLeaseUnfenced,
				lease.id,
				revokeErr,
			)
		}
		if err != nil {
			return err
		}
		return nil
	}
	// A successful lease transition preserves the existing generation. This
	// store also clears the fail-closed invalid bit set on keepalive loss.
	sd.publishSelfOwnerEpoch(sd.server.OwnerEpoch())
	releasePublication()
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
		revision := response.Header.GetRevision()
		sd.rememberSelfRegistration(payload, revision)
		sd.publishSelfOwnerEpoch(uint64(revision))
	}
	return response.Succeeded, nil
}

func (sd *etcdServiceDiscovery) rememberSelfRegistration(payload string, revision int64) {
	candidate := &selfRegistrationSnapshot{payload: payload, revision: revision}
	for {
		current := sd.lease.registration.Load()
		if current != nil && current.revision > revision {
			return
		}
		if sd.lease.registration.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func (sd *etcdServiceDiscovery) currentSelfRegistrationPayload() string {
	if current := sd.lease.registration.Load(); current != nil {
		return current.payload
	}
	return sd.server.AsJSONString()
}

// reconcileCandidateRegistration resolves a transaction with an ambiguous
// client result. Observing the candidate lease proves the transaction committed.
// Otherwise revoking that lease fences a request which may still linearize.
func (sd *etcdServiceDiscovery) reconcileCandidateRegistration(candidateLeaseID clientv3.LeaseID) (bool, error) {
	getCtx, cancelGet := withOptionalTimeout(sd.lifecycle.ctx, sd.etcdDialTimeout)
	response, getErr := sd.cli.Get(getCtx, getKey(sd.server.ID, sd.server.Type))
	cancelGet()
	if getErr == nil && len(response.Kvs) == 1 && clientv3.LeaseID(response.Kvs[0].Lease) == candidateLeaseID {
		sd.rememberSelfRegistration(string(response.Kvs[0].Value), response.Kvs[0].ModRevision)
		sd.publishSelfOwnerEpoch(uint64(response.Kvs[0].CreateRevision))
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
	// Candidate leases are not yet represented by leaseID. Their cleanup must
	// survive lifecycle cancellation or Shutdown has no other way to find them.
	revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), sd.etcdDialTimeout)
	defer cancelRevoke()
	_, err := sd.cli.Revoke(revokeCtx, candidateLeaseID)
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	return err
}

func (sd *etcdServiceDiscovery) requestRenewalSync() {
	select {
	case sd.lease.renewal <- struct{}{}:
	default:
	}
}

func (sd *etcdServiceDiscovery) runRenewalSync(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sd.lease.renewal:
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
	l, err := sd.cli.Grant(grantCtx, int64(sd.lease.heartbeatTTL.Seconds()))
	cancelGrant()
	if err != nil {
		return nil, err
	}
	logger.Zap.Debug("sd: got leaseID", zap.Int64("leaseID", int64(l.ID)), zap.String("self", sd.server.ID))
	// this will keep alive forever, when channel c is closed
	// it means we probably have to rebootstrap the lease
	keepAliveCtx, cancelKeepAlive := context.WithCancel(sd.lease.ctx)
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
	sd.lifecycle.mu.Lock()
	defer sd.lifecycle.mu.Unlock()
	if (sd.lifecycle.state != etcdLifecycleInitializing && sd.lifecycle.state != etcdLifecycleRunning) || sd.lifecycle.ctx.Err() != nil {
		err := sd.lifecycle.ctx.Err()
		if err == nil {
			err = context.Canceled
		}
		return false, err
	}
	if expectedLeaseID != 0 && clientv3.LeaseID(sd.lease.id.Load()) != expectedLeaseID {
		return false, nil
	}
	sd.lifecycle.wg.Add(1)
	sd.lease.id.Store(int64(lease.id))
	go func() {
		defer sd.lifecycle.wg.Done()
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
	return sd.flushServerWithLeaseContext(sd.lease.flushCtx, server)
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
		leaseID := clientv3.LeaseID(sd.lease.id.Load())
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
	return sd.addServerIntoEtcdContext(sd.lifecycle.ctx, server)
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
	return sd.bootstrapServerWithLeaseContext(ctx, server, clientv3.LeaseID(sd.lease.id.Load()))
}

func (sd *etcdServiceDiscovery) bootstrapServer(server *Server) error {
	if err := sd.bootstrapServerContext(sd.lifecycle.ctx, server); err != nil {
		return err
	}
	_ = sd.syncServers(sd.lifecycle.ctx, true, true)
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
	key := getKey(serverID, serverType)
	response, err := sd.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, payload, clientv3.WithLease(leaseID))).
		Else(clientv3.OpGet(key)).
		Commit()
	if err != nil {
		return err
	}
	ownerEpoch := response.Header.GetRevision()
	modRevision := response.Header.GetRevision()
	if !response.Succeeded {
		if len(response.Responses) != 1 {
			return errors.New("etcd server identity check returned an invalid response")
		}
		rangeResponse := response.Responses[0].GetResponseRange()
		if rangeResponse == nil || len(rangeResponse.Kvs) != 1 {
			return fmt.Errorf("%w: %s/%s changed during registration", errEtcdServerIdentityOwned, serverType, serverID)
		}
		existing := rangeResponse.Kvs[0]
		if clientv3.LeaseID(existing.Lease) != leaseID {
			return fmt.Errorf(
				"%w: %s/%s is attached to lease %x",
				errEtcdServerIdentityOwned,
				serverType,
				serverID,
				existing.Lease,
			)
		}
		update, updateErr := sd.cli.Txn(ctx).
			If(
				clientv3.Compare(clientv3.ModRevision(key), "=", existing.ModRevision),
				clientv3.Compare(clientv3.LeaseValue(key), "=", int64(leaseID)),
			).
			Then(clientv3.OpPut(key, payload, clientv3.WithLease(leaseID))).
			Commit()
		if updateErr != nil {
			return updateErr
		}
		if !update.Succeeded {
			return fmt.Errorf("%w: %s/%s changed during refresh", errEtcdServerIdentityOwned, serverType, serverID)
		}
		ownerEpoch = existing.CreateRevision
		modRevision = update.Header.GetRevision()
	}
	if serverID == sd.server.ID && serverType == sd.server.Type {
		sd.rememberSelfRegistration(payload, modRevision)
		sd.publishSelfOwnerEpoch(uint64(ownerEpoch))
	}
	return nil
}

// AddListener adds a listener to etcd service discovery
func (sd *etcdServiceDiscovery) AddListener(listener SDListener) {
	sd.watch.listenersMu.Lock()
	defer sd.watch.listenersMu.Unlock()
	sd.watch.listeners = append(sd.watch.listeners, listener)
}

// OnLeaderChange 设置leader变化监听
func (sd *etcdServiceDiscovery) OnLeaderChange(listener LeaderChangeListener) {
	sd.election.leaderMu.Lock()
	defer sd.election.leaderMu.Unlock()
	sd.election.listener = listener
}

func (sd *etcdServiceDiscovery) activateLeaderGeneration(generation uint64) bool {
	sd.election.admissionMu.Lock()
	defer sd.election.admissionMu.Unlock()
	if generation <= sd.election.generation {
		return false
	}
	sd.election.generation = generation
	return true
}

func (sd *etcdServiceDiscovery) notifyLeaderChange(generation uint64, leader string, initial bool) bool {
	// Pair the generation check with callback admission. A stale generation
	// cannot wait on a full lane and enter it after the coordinator has moved on.
	sd.election.admissionMu.Lock()
	defer sd.election.admissionMu.Unlock()
	if generation != sd.election.generation {
		return false
	}
	_, accepted := sd.enqueueLeaderCallback(func() {
		// Publish later transitions and invoke the listener in the same FIFO work.
		// Some listeners query IsLeader instead of trusting the callback argument.
		sd.election.leaderMu.Lock()
		sd.election.leaderID = leader
		listener := sd.election.listener
		sd.election.leaderMu.Unlock()
		if listener != nil {
			listener(leader)
		}
	})
	if accepted && initial {
		// The first non-empty leader is visible before Init returns. Its listener
		// remains asynchronous, and later state still advances inside the FIFO lane.
		sd.election.leaderMu.Lock()
		sd.election.leaderID = leader
		sd.election.leaderMu.Unlock()
	}
	return accepted
}

// AfterInit executes after Init
func (sd *etcdServiceDiscovery) AfterInit() {
}

func (sd *etcdServiceDiscovery) notifyListeners(act Action, sv *Server, old ...*Server) {
	sd.watch.listenersMu.RLock()
	listeners := append([]SDListener(nil), sd.watch.listeners...)
	sd.watch.listenersMu.RUnlock()
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
	sd.watch.byTypeMu.Lock()
	defer sd.watch.byTypeMu.Unlock()
	f()
}

func (sd *etcdServiceDiscovery) getConsistentHash(serverType string) *hash.ConsistentHash {
	sd.watch.consistentHashMu.RLock()
	ha := sd.watch.consistentHash[serverType]
	sd.watch.consistentHashMu.RUnlock()
	return ha
}

func (sd *etcdServiceDiscovery) getOrCreateConsistentHash(serverType string) *hash.ConsistentHash {
	sd.watch.consistentHashMu.Lock()
	defer sd.watch.consistentHashMu.Unlock()

	ha := sd.watch.consistentHash[serverType]
	if ha == nil {
		ha = hash.NewConsistentHash()
		sd.watch.consistentHash[serverType] = ha
	}
	return ha
}

func (sd *etcdServiceDiscovery) deleteServer(serverID string) {
	if actual, ok := sd.watch.byID.Load(serverID); ok {
		sv := actual.(*Server)
		sd.watch.byID.Delete(sv.ID)
		sd.writeLockScope(func() {
			if svMap, ok := sd.watch.byType[sv.Type]; ok {
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
	sd.watch.byID.Range(func(key interface{}, value interface{}) bool {
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
	if revision > 0 && revision <= sd.watch.revisions[server.ID] {
		return false
	}
	if revision > 0 {
		sd.watch.revisions[server.ID] = revision
	}
	// Self fencing state is published only by transactions proven to use the
	// local lease. Generic topology data may describe a replacement process with
	// the same identity and must never be adopted by the old process.
	sd.addServer(server)
	return true
}

func (sd *etcdServiceDiscovery) deleteServerAtRevision(serverID string, revision int64) bool {
	if revision > 0 && revision <= sd.watch.revisions[serverID] {
		return false
	}
	if revision > 0 {
		sd.watch.revisions[serverID] = revision
	}
	sd.deleteServer(serverID)
	return true
}

func (sd *etcdServiceDiscovery) deleteLocalInvalidServersAtRevision(actualServers map[string]struct{}, snapshotRevision int64) {
	sd.watch.byID.Range(func(key interface{}, value interface{}) bool {
		serverID := key.(string)
		if _, ok := actualServers[serverID]; ok {
			return true
		}
		if snapshotRevision > 0 && sd.watch.revisions[serverID] > snapshotRevision {
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
	for serverID, revision := range sd.watch.revisions {
		if revision > snapshotRevision {
			continue
		}
		if _, loaded := sd.watch.byID.Load(serverID); loaded {
			continue
		}
		delete(sd.watch.revisions, serverID)
	}
}

func (sd *etcdServiceDiscovery) pruneServerRevisionCandidates(serverIDs map[string]struct{}) {
	for serverID := range serverIDs {
		if _, loaded := sd.watch.byID.Load(serverID); loaded {
			continue
		}
		if revision, ok := sd.watch.revisions[serverID]; ok && revision <= sd.watch.revisionFloor {
			delete(sd.watch.revisions, serverID)
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
	payload, revision, ownerEpoch, err := getServerValueFromEtcdContext(ctx, cli, serverType, serverID, atRevision)
	if err != nil {
		return nil, 0, err
	}
	server, err := parseServerForKey(payload, serverType, serverID)
	if err != nil {
		return nil, 0, err
	}
	server.SetOwnerEpoch(uint64(ownerEpoch))
	return server, revision, nil
}

func getServerValueFromEtcdContext(ctx context.Context, cli *clientv3.Client, serverType, serverID string, atRevision int64) ([]byte, int64, int64, error) {
	svKey := getKey(serverID, serverType)
	var options []clientv3.OpOption
	if atRevision > 0 {
		options = append(options, clientv3.WithRev(atRevision))
	}
	svEInfo, err := cli.Get(ctx, svKey, options...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("error getting server %s from etcd: %w", svKey, err)
	}
	if len(svEInfo.Kvs) == 0 {
		return nil, 0, 0, fmt.Errorf("didn't found server: %s in etcd", svKey)
	}
	return svEInfo.Kvs[0].Value, svEInfo.Kvs[0].ModRevision, svEInfo.Kvs[0].CreateRevision, nil
}

// GetServersByType returns a slice with all the servers of a certain type
func (sd *etcdServiceDiscovery) GetServersByType(serverType string) (map[string]*Server, error) {
	sd.watch.byTypeMu.RLock()
	defer sd.watch.byTypeMu.RUnlock()
	if m, ok := sd.watch.byType[serverType]; ok && len(m) > 0 {
		// Create a new map to avoid concurrent read and write access to the
		// map, this also prevents accidental changes to the list of servers
		// kept by the service discovery.
		ret := make(map[string]*Server, len(sd.watch.byType[serverType]))
		for k, v := range sd.watch.byType[serverType] {
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
	sd.watch.byID.Range(func(k, v interface{}) bool {
		ret = append(ret, v.(*Server))
		return true
	})
	return ret
}

func (sd *etcdServiceDiscovery) GetAnyFrontend() (*Server, error) {
	var frontend *Server
	sd.watch.byID.Range(func(k, v interface{}) bool {
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
	sd.watch.byTypeMu.RLock()
	defer sd.watch.byTypeMu.RUnlock()
	ret := make(map[string]*Server, len(sd.watch.byType))
	for t, m := range sd.watch.byType {
		for _, server := range m {
			ret[t] = server
			break
		}
	}
	return ret
}

func (sd *etcdServiceDiscovery) bootstrap() error {
	sd.server.InvalidateOwnerEpoch()
	if err := sd.grantLease(sd.lease.ctx); err != nil {
		return err
	}

	// 选举
	if sd.election.enabled {
		ctx, cancel := context.WithCancel(sd.lifecycle.ctx)
		if !sd.installElectionCancel(cancel) {
			cancel()
			return sd.lifecycle.ctx.Err()
		}
		if err := sd.runElection(ctx); err != nil {
			cancel()
			return err
		}
	}

	if err := sd.bootstrapServerContext(sd.lifecycle.ctx, sd.server); err != nil {
		return err
	}
	sd.watch.reconcileMu.Lock()
	sd.applyServerAtRevision(sd.server, 0)
	sd.watch.reconcileMu.Unlock()
	// Registration has historically been the fatal bootstrap boundary. A
	// transient initial read must not turn a registered production server into
	// a terminal Init failure; the periodic sync and watch paths will converge.
	sd.lifecycle.waitingCallbacks.Store(true)
	_ = sd.syncServers(sd.lifecycle.ctx, true, true)
	sd.lifecycle.waitingCallbacks.Store(false)

	return nil
}

// GetServer returns a server given it's id
func (sd *etcdServiceDiscovery) GetServer(id string) (*Server, error) {
	if sv, ok := sd.watch.byID.Load(id); ok {
		return sv.(*Server), nil
	}
	return nil, errors.WithStack(fmt.Errorf("%w:%s", constants.ErrNoServerWithID, id))
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
	sd.watch.byTypeMu.RLock()
	defer sd.watch.byTypeMu.RUnlock()
	for k, v := range sd.watch.byType {
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
	ownerEpoch     int64
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
			err := fmt.Errorf("panic loading server %s/%s from etcd", work.serverType, work.serverID)
			util.LogRecoveredPanic(
				logger.Zap,
				"panic loading server from etcd",
				recovered,
				zap.String("svType", work.serverType),
				zap.String("svID", work.serverID),
				zap.Stack("stack"),
			)
			p.recordError(err)
		}
	}()

	logger.Zap.Debug("loading info from missing server", zap.String("svType", work.serverType), zap.String("svID", work.serverID))
	payload := work.payload
	revision := work.revision
	ownerEpoch := work.ownerEpoch
	var err error
	if !work.payloadPresent {
		var fetchedEpoch int64
		payload, revision, fetchedEpoch, err = getServerValueFromEtcdContext(p.ctx, p.cli, work.serverType, work.serverID, work.readAt)
		if ownerEpoch == 0 {
			ownerEpoch = fetchedEpoch
		}
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
	if sv != nil {
		sv.SetOwnerEpoch(uint64(ownerEpoch))
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

func (p *parallelGetter) addWorkWithPayload(serverType, serverID string, payload []byte, revision int64, ownerEpochValues ...int64) {
	var ownerEpoch int64
	if len(ownerEpochValues) > 0 {
		ownerEpoch = ownerEpochValues[0]
	}
	p.wg.Add(1)
	p.workChan <- parallelGetterWork{
		serverType:     serverType,
		serverID:       serverID,
		payload:        payload,
		payloadPresent: true,
		revision:       revision,
		ownerEpoch:     ownerEpoch,
	}
}

func (p *parallelGetter) addWork(serverType, serverID string, readAt int64, ownerEpochValues ...int64) {
	var ownerEpoch int64
	if len(ownerEpochValues) > 0 {
		ownerEpoch = ownerEpochValues[0]
	}
	p.wg.Add(1)
	p.workChan <- parallelGetterWork{
		serverType: serverType,
		serverID:   serverID,
		readAt:     readAt,
		ownerEpoch: ownerEpoch,
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

	sd.watch.reconcileMu.Lock()
	appliedRevisions := make(map[string]int64, len(sd.watch.revisions))
	for serverID, revision := range sd.watch.revisions {
		appliedRevisions[serverID] = revision
	}
	sd.watch.reconcileMu.Unlock()

	snapshot := &etcdServerSnapshot{
		revision:  kvs.Header.Revision,
		serverIDs: make(map[string]struct{}, len(kvs.Kvs)),
	}
	parallelGetter, err := newParallelGetter(ctx, sd.cli, sd.watch.parallelism)
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
		_, loaded := sd.watch.byID.Load(svID)
		appliedRevision := appliedRevisions[svID]
		if appliedRevision > kv.ModRevision || (loaded && appliedRevision == kv.ModRevision) {
			continue
		}
		if fullValues {
			parallelGetter.addWorkWithPayload(svType, svID, kv.Value, kv.ModRevision, kv.CreateRevision)
		} else {
			// Read the payload from the keys-only snapshot revision. A current
			// read could mix a later PUT with the earlier deletion set.
			parallelGetter.addWork(svType, svID, snapshot.revision, kv.CreateRevision)
		}
	}

	snapshot.servers, err = parallelGetter.waitAndGetResult()
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (sd *etcdServiceDiscovery) applyServerSnapshot(snapshot *etcdServerSnapshot, waitCallbacks bool) (<-chan struct{}, bool) {
	sd.watch.reconcileMu.Lock()
	if sd.watch.revisionFloor == 0 || snapshot.revision > sd.watch.revisionFloor {
		for _, versioned := range snapshot.servers {
			if versioned.server == nil {
				if versioned.revision > sd.watch.revisions[versioned.serverID] {
					sd.watch.revisions[versioned.serverID] = versioned.revision
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
			sd.watch.revisionFloor = snapshot.revision
		}
		sd.pruneServerRevisionTombstones(snapshot.revision)
		if snapshot.revision > sd.watch.watchRevision {
			// Full snapshots carry every payload. Keys-only snapshots reach this
			// point only after every missing or lagging payload was fetched at the
			// same revision, so both forms may safely advance the checkpoint.
			sd.watch.watchRevision = snapshot.revision
		}
	}
	sd.watch.lastSync.Store(time.Now().UnixNano())
	var callbackBarrier <-chan struct{}
	callbackAccepted := true
	if waitCallbacks {
		// Public SyncServers keeps its historical completion boundary: all
		// listener work admitted before this marker finishes before return. A
		// callback running elsewhere is never a reason to skip this marker.
		callbackBarrier, callbackAccepted = sd.enqueueCallback(nil)
	}
	sd.watch.reconcileMu.Unlock()
	return callbackBarrier, callbackAccepted
}

// SyncServers gets all servers from etcd
func (sd *etcdServiceDiscovery) SyncServers(firstSync bool) error {
	return sd.syncServers(sd.lifecycle.ctx, firstSync, true)
}

func (sd *etcdServiceDiscovery) syncServers(ctx context.Context, firstSync, waitCallbacks bool) error {
	sd.watch.fetchMu.Lock()
	snapshot, err := sd.fetchServerSnapshot(ctx, firstSync)
	if err != nil && !firstSync && errors.Is(err, rpctypes.ErrCompacted) {
		// A keys-only snapshot can be compacted before its historical payload
		// reads complete. Fall back to one current full-value snapshot.
		snapshot, err = sd.fetchServerSnapshot(ctx, true)
	}
	if err != nil {
		sd.watch.fetchMu.Unlock()
		if ctx.Err() == nil {
			logger.Zap.Error("Error querying etcd server", zap.Error(err))
		}
		return err
	}
	callbackBarrier, callbackAccepted := sd.applyServerSnapshot(snapshot, waitCallbacks)
	sd.watch.fetchMu.Unlock()
	return sd.watch.callbacks.waitForBarrier(ctx, callbackBarrier, callbackAccepted)
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
		time.Sleep(sd.shutdownDelay) // Sleep for a short while to ensure shutdown has propagated
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

// revoke prevents Pitaya from crashing when etcd is not available
func (sd *etcdServiceDiscovery) revoke() error {
	sd.server.InvalidateOwnerEpoch()
	ctx, cancel := context.WithTimeout(context.Background(), sd.lease.revokeTimeout)
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

	_, err = sd.cli.Revoke(ctx, clientv3.LeaseID(sd.lease.id.Load()))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		logger.Zap.Warn("timed out waiting for etcd revoke", zap.String("self", sd.server.ID))
		return nil // timed out
	}
	logger.Zap.Debug("finished waiting for etcd revoke", zap.String("self", sd.server.ID))
	return err
}

func (sd *etcdServiceDiscovery) addServer(sv *Server) {
	old, loaded := sd.watch.byID.Load(sv.ID)
	sd.writeLockScope(func() {
		sd.watch.byID.Store(sv.ID, sv)
		mapSvByType, ok := sd.watch.byType[sv.Type]
		if !ok {
			mapSvByType = make(map[string]*Server)
			sd.watch.byType[sv.Type] = mapSvByType
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
		sd.watch.reconcileMu.Lock()
		watchRevision := sd.watch.watchRevision
		sd.watch.reconcileMu.Unlock()
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
	sd.watch.reconcileMu.Lock()
	if sd.watch.watchRevision == 0 {
		sd.watch.watchRevision = createdRevision
	}
	sd.watch.reconcileMu.Unlock()
}

// applyEtcdWatchResponse returns false when the response cannot form a
// contiguous checkpoint. The caller must replace that watch with a complete
// snapshot before accepting later responses.
func (sd *etcdServiceDiscovery) applyEtcdWatchResponse(watchResponse clientv3.WatchResponse) bool {
	if watchResponse.Created {
		return len(watchResponse.Events) == 0
	}
	if len(watchResponse.Events) == 0 {
		sd.watch.reconcileMu.Lock()
		if watchResponse.Header.Revision > sd.watch.watchRevision {
			sd.watch.watchRevision = watchResponse.Header.Revision
		}
		if sd.watch.revisionFloor > 0 && watchResponse.Header.Revision > sd.watch.revisionFloor {
			sd.watch.revisionFloor = watchResponse.Header.Revision
			sd.pruneServerRevisionTombstones(watchResponse.Header.Revision)
		}
		sd.watch.reconcileMu.Unlock()
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

	sd.watch.reconcileMu.Lock()
	defer sd.watch.reconcileMu.Unlock()
	pruneCandidates := make(map[string]struct{}, len(watchResponse.Events))
	for _, ev := range watchResponse.Events {
		revision := ev.Kv.ModRevision
		if revision <= sd.watch.revisionFloor {
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
				if revision > sd.watch.revisions[svID] {
					// Consume the malformed remote revision while preserving the
					// last valid local value. A later valid PUT can still recover it.
					sd.watch.revisions[svID] = revision
				}
				continue
			}
			sv.SetOwnerEpoch(uint64(ev.Kv.CreateRevision))

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
	if responseRevision > sd.watch.watchRevision {
		sd.watch.watchRevision = responseRevision
	}
	if sd.watch.revisionFloor > 0 && responseRevision > sd.watch.revisionFloor {
		sd.watch.revisionFloor = responseRevision
		sd.pruneServerRevisionCandidates(pruneCandidates)
	}
	return true
}

func (sd *etcdServiceDiscovery) isServerTypeBlacklisted(svType string) bool {
	for _, blacklistedSv := range sd.watch.blacklist {
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

type electionLeaseProvenance uint8

const (
	electionLeaseInvalid electionLeaseProvenance = iota
	electionLeaseService
	electionLeaseLocal
	electionLeaseBorrowed
)

type electionLease struct {
	id         clientv3.LeaseID
	provenance electionLeaseProvenance
}

type electionGeneration struct {
	token    uint64
	ctx      context.Context
	cancel   context.CancelFunc
	session  *concurrency.Session
	election *concurrency.Election
	lease    electionLease
}

func (generation *electionGeneration) reconnectLease(id clientv3.LeaseID) electionLease {
	if id == 0 {
		return electionLease{provenance: electionLeaseLocal}
	}
	if id == generation.lease.id {
		return generation.lease
	}
	return electionLease{id: id, provenance: electionLeaseBorrowed}
}

func (sd *etcdServiceDiscovery) runElection(ctx context.Context) error {
	const initialGeneration uint64 = 1
	generation, err := sd.newElectionGeneration(ctx, electionLease{
		id:         clientv3.LeaseID(sd.lease.id.Load()),
		provenance: electionLeaseService,
	}, initialGeneration)
	if err != nil {
		return errors.Wrap(err, "while creating initial session")
	}
	if !sd.activateLeaderGeneration(initialGeneration) {
		sd.stopElectionGeneration(generation, false)
		return errors.New("while activating initial election generation")
	}

	leaderReady := make(chan struct{})
	if !sd.startLifecycleWorker(func() { sd.runElectionCoordinator(ctx, generation, leaderReady) }) {
		sd.stopElectionGeneration(generation, false)
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("while starting election coordinator")
	}

	select {
	case <-leaderReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sd *etcdServiceDiscovery) runElectionCoordinator(
	ctx context.Context,
	generation *electionGeneration,
	leaderReady chan struct{},
) {
	var leader string
	var pendingLease electionLease
	defer func() { sd.revokeElectionLease(pendingLease, false) }()
	initialPublished := false
	publish := func(next string) bool {
		if leader == next {
			logger.Zap.Debug("etcd election leader remains the same,ignore", zap.String("leaderID", leader), zap.String("self", sd.server.ID))
			return true
		}
		initial := !initialPublished && next != ""
		if !sd.notifyLeaderChange(generation.token, next, initial) {
			return false
		}
		logger.Zap.Info("etcd election leader changed", zap.String("name", sd.election.name),
			zap.Uint64("generation", generation.token), zap.String("leaderID", next),
			zap.String("old", leader), zap.String("self", sd.server.ID))
		leader = next
		if initial {
			initialPublished = true
			close(leaderReady)
		}
		return true
	}

	for {
		nextLeaseID, reconnect := sd.runElectionGeneration(ctx, generation, publish)
		nextLease := generation.reconnectLease(nextLeaseID)
		currentLease := generation.lease
		sd.detachElectionGeneration(generation)
		pendingLease = currentLease
		if !reconnect || ctx.Err() != nil {
			return
		}

		logger.Zap.Debug("etcd election need reconnect", zap.String("self", sd.server.ID))
		nextGeneration := generation.token + 1
		if nextGeneration == 0 || !sd.activateLeaderGeneration(nextGeneration) {
			return
		}
		generation.token = nextGeneration
		if leader != "" && !publish("") {
			return
		}
		preserveLease := nextLease == currentLease
		sd.revokeElectionLease(currentLease, preserveLease)
		pendingLease = nextLease
		if (nextLease.id == 0 || preserveLease) && !waitForContext(ctx, sd.election.reconnectDelay) {
			return
		}
		for {
			next, err := sd.newElectionGeneration(ctx, nextLease, nextGeneration)
			if err == nil {
				generation = next
				pendingLease = electionLease{}
				break
			}
			if ctx.Err() != nil {
				return
			}
			logger.Zap.Error("while creating new session", zap.String("self", sd.server.ID),
				zap.Int64("lease", int64(nextLease.id)), zap.Error(err))
			if !waitForContext(ctx, sd.election.reconnectDelay) {
				return
			}
		}
	}
}

func (sd *etcdServiceDiscovery) runElectionGeneration(
	ctx context.Context,
	generation *electionGeneration,
	publish func(string) bool,
) (clientv3.LeaseID, bool) {
	sessionCtx := generation.session.Ctx()
	reconnectLeaseAfterReadError := func() clientv3.LeaseID {
		if sessionCtx.Err() != nil {
			return 0
		}
		return generation.session.Lease()
	}
	readCurrentLeader := func() (*clientv3.GetResponse, error) {
		readCtx, cancel := withOptionalTimeout(sessionCtx, sd.electionShutdownTimeout())
		defer cancel()
		return generation.election.Leader(readCtx)
	}
	node, err := readCurrentLeader()
	resumeObservation := false
	var initialLeader *clientv3.GetResponse
	if err != nil && !errors.Is(err, concurrency.ErrElectionNoLeader) {
		if ctx.Err() == nil {
			logger.Zap.Error("while determining election leader", zap.String("self", sd.server.ID), zap.Error(err))
		}
		return reconnectLeaseAfterReadError(), ctx.Err() == nil
	}
	if err == nil && len(node.Kvs) > 0 {
		initialLeader = node
		if string(node.Kvs[0].Value) == sd.server.ID {
			leaderLease := clientv3.LeaseID(node.Kvs[0].Lease)
			if generation.session.Lease() != leaderLease {
				return leaderLease, true
			}
			resumed := concurrency.ResumeElection(generation.session, sd.election.name+"/",
				string(node.Kvs[0].Key), node.Kvs[0].CreateRevision)
			generation.election = resumed
			if sd.election.resumeLeader {
				resumeObservation = true
			} else {
				resignCtx, cancel := context.WithTimeout(generation.ctx, sd.electionShutdownTimeout())
				err = resumed.Resign(resignCtx)
				cancel()
				initialLeader = nil
				if err != nil {
					logger.Zap.Error("while resigning leadership after reconnect", zap.String("self", sd.server.ID), zap.Error(err))
					if errors.Is(err, rpctypes.ErrLeaseNotFound) {
						return 0, ctx.Err() == nil
					}
					return generation.session.Lease(), ctx.Err() == nil
				}
			}
		}
	}

	observeCtx, cancelObserve := context.WithCancel(generation.ctx)
	observe := generation.election.Observe(observeCtx)
	stopObservation := func() {
		cancelObserve()
		for range observe {
		}
	}
	type leaderKeyIdentity struct {
		key            string
		createRevision int64
	}
	var leaderDeleteWatch clientv3.WatchChan
	var cancelLeaderDeleteWatch context.CancelFunc
	var watchedLeader leaderKeyIdentity
	stopLeaderDeleteWatch := func() {
		if cancelLeaderDeleteWatch == nil {
			return
		}
		cancelLeaderDeleteWatch()
		if leaderDeleteWatch != nil {
			for range leaderDeleteWatch {
			}
		}
		leaderDeleteWatch = nil
		cancelLeaderDeleteWatch = nil
		watchedLeader = leaderKeyIdentity{}
	}
	installLeaderDeleteWatch := func(key string, createRevision, snapshotRevision int64) bool {
		if key == "" || createRevision <= 0 || snapshotRevision <= 0 {
			return false
		}
		identity := leaderKeyIdentity{key: key, createRevision: createRevision}
		if cancelLeaderDeleteWatch != nil && watchedLeader == identity {
			return true
		}
		stopLeaderDeleteWatch()
		watchCtx, cancel := context.WithCancel(generation.ctx)
		watch := generation.session.Client().Watch(watchCtx, key,
			clientv3.WithRev(snapshotRevision+1), clientv3.WithFilterPut())
		if watch == nil {
			cancel()
			return false
		}
		leaderDeleteWatch = watch
		cancelLeaderDeleteWatch = cancel
		watchedLeader = identity
		return true
	}
	installLeaderResponseWatch := func(response *clientv3.GetResponse) bool {
		if response == nil || response.Header == nil || len(response.Kvs) == 0 {
			return false
		}
		kv := response.Kvs[0]
		return installLeaderDeleteWatch(string(kv.Key), kv.CreateRevision, response.Header.Revision)
	}
	stopGenerationObservation := func() {
		stopLeaderDeleteWatch()
		stopObservation()
	}

	var campaignDone chan error
	var cancelCampaign context.CancelFunc
	startCampaign := func() {
		if campaignDone != nil {
			return
		}
		campaignCtx, cancel := context.WithCancel(generation.ctx)
		done := make(chan error, 1)
		election := generation.election
		cancelCampaign = cancel
		campaignDone = done
		go func() {
			done <- election.Campaign(campaignCtx, sd.server.ID)
		}()
	}
	stopCampaign := func() error {
		if campaignDone == nil {
			return nil
		}
		cancelCampaign()
		err := <-campaignDone
		cancelCampaign = nil
		campaignDone = nil
		return err
	}
	if initialLeader != nil && !installLeaderResponseWatch(initialLeader) {
		generation.cancel()
		stopGenerationObservation()
		return generation.session.Lease(), ctx.Err() == nil
	}

	ownsLeadership := resumeObservation
	// A resumed snapshot already establishes self ownership. Observe guarantees
	// the most recent leader, not replay of that snapshot, so its first value may
	// legitimately be another candidate and must restart Campaign.
	recampaignPending := false
	var lastDeletedLeader leaderKeyIdentity
	var deletedRevisionFloor int64
	if !resumeObservation {
		startCampaign()
	}

	for {
		select {
		case resp, ok := <-observe:
			if !ok {
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				if ctx.Err() == nil {
					logger.Zap.Error("election observe stream closed", zap.String("self", sd.server.ID))
				}
				return generation.session.Lease(), ctx.Err() == nil
			}
			if len(resp.Kvs) == 0 {
				continue
			}
			if resp.Header == nil || resp.Header.Revision <= 0 || resp.Kvs[0].CreateRevision <= 0 {
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				if ctx.Err() == nil {
					logger.Zap.Error("invalid election observe response", zap.String("self", sd.server.ID))
				}
				return generation.session.Lease(), ctx.Err() == nil
			}
			observedLeader := leaderKeyIdentity{
				key:            string(resp.Kvs[0].Key),
				createRevision: resp.Kvs[0].CreateRevision,
			}
			if resp.Header.Revision < deletedRevisionFloor || observedLeader == lastDeletedLeader {
				logger.Zap.Debug("ignoring deleted election leader observation", zap.String("self", sd.server.ID),
					zap.Uint64("generation", generation.token), zap.String("key", observedLeader.key),
					zap.Int64("createRevision", observedLeader.createRevision),
					zap.Int64("responseRevision", resp.Header.Revision),
					zap.Int64("deletedRevisionFloor", deletedRevisionFloor))
				continue
			}
			leader := string(resp.Kvs[0].Value)
			leaderLease := clientv3.LeaseID(resp.Kvs[0].Lease)
			currentLeader, leaderErr := readCurrentLeader()
			if errors.Is(leaderErr, concurrency.ErrElectionNoLeader) {
				logger.Zap.Debug("ignoring election leader observation with no current leader",
					zap.String("self", sd.server.ID), zap.Uint64("generation", generation.token),
					zap.String("key", observedLeader.key),
					zap.Int64("createRevision", observedLeader.createRevision))
				continue
			}
			if leaderErr != nil {
				nextLease := reconnectLeaseAfterReadError()
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				if ctx.Err() == nil {
					logger.Zap.Error("while rechecking observed election leader",
						zap.String("self", sd.server.ID), zap.Error(leaderErr))
				}
				return nextLease, ctx.Err() == nil
			}
			if currentLeader == nil || currentLeader.Header == nil ||
				len(currentLeader.Kvs) == 0 || currentLeader.Kvs[0].CreateRevision <= 0 {
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				if ctx.Err() == nil {
					logger.Zap.Error("invalid current election leader response",
						zap.String("self", sd.server.ID))
				}
				return generation.session.Lease(), ctx.Err() == nil
			}
			currentKV := currentLeader.Kvs[0]
			if string(currentKV.Key) != observedLeader.key ||
				currentKV.CreateRevision != observedLeader.createRevision ||
				string(currentKV.Value) != leader ||
				clientv3.LeaseID(currentKV.Lease) != leaderLease {
				logger.Zap.Debug("ignoring non-current election leader observation",
					zap.String("self", sd.server.ID), zap.Uint64("generation", generation.token),
					zap.String("key", observedLeader.key),
					zap.Int64("createRevision", observedLeader.createRevision),
					zap.Int64("responseRevision", resp.Header.Revision),
					zap.String("currentKey", string(currentKV.Key)),
					zap.Int64("currentCreateRevision", currentKV.CreateRevision))
				continue
			}
			logger.Zap.Debug("etcd election observe", zap.String("self", sd.server.ID),
				zap.Uint64("generation", generation.token), zap.String("leader", leader),
				zap.Int64("leaderLease", int64(leaderLease)))
			if leader == sd.server.ID && leaderLease != generation.session.Lease() {
				// Adopt the observed lease as borrowed in a new generation before
				// publishing self ownership. Cleanup must never revoke that lease.
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				return leaderLease, ctx.Err() == nil
			}
			if !installLeaderResponseWatch(currentLeader) {
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				return generation.session.Lease(), ctx.Err() == nil
			}
			if !publish(leader) {
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				return 0, false
			}
			if leader == sd.server.ID {
				ownsLeadership = true
				recampaignPending = false
			} else {
				// Observe may skip a short-lived self proposal. Record every foreign
				// leader so both Campaign-first and Observe-first orderings leave a
				// fresh proposal behind the replacement.
				ownsLeadership = false
				if campaignDone == nil {
					startCampaign()
				} else {
					recampaignPending = true
				}
			}
		case campaignErr := <-campaignDone:
			cancelCampaign()
			cancelCampaign = nil
			campaignDone = nil
			if campaignErr != nil {
				generation.cancel()
				stopGenerationObservation()
				if ctx.Err() == nil {
					logger.Zap.Error("while campaigning for leader", zap.String("self", sd.server.ID), zap.Error(campaignErr))
				}
				if errors.Is(campaignErr, rpctypes.ErrLeaseNotFound) {
					return 0, ctx.Err() == nil
				}
				return generation.session.Lease(), ctx.Err() == nil
			}
			if recampaignPending {
				recampaignPending = false
				ownsLeadership = false
				startCampaign()
			} else {
				header := generation.election.Header()
				if header == nil || !installLeaderDeleteWatch(generation.election.Key(),
					generation.election.Rev(), header.Revision) {
					generation.cancel()
					stopGenerationObservation()
					return generation.session.Lease(), ctx.Err() == nil
				}
				ownsLeadership = true
			}
		case watchResponse, ok := <-leaderDeleteWatch:
			if !ok || watchResponse.Err() != nil {
				watchErr := watchResponse.Err()
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				if ctx.Err() == nil {
					logger.Zap.Error("election leader delete watch closed", zap.String("self", sd.server.ID),
						zap.Error(watchErr))
				}
				return generation.session.Lease(), ctx.Err() == nil
			}
			deleted := false
			deleteRevision := int64(0)
			for _, event := range watchResponse.Events {
				if event.Type != clientv3.EventTypeDelete || string(event.Kv.Key) != watchedLeader.key {
					continue
				}
				deleted = true
				if event.Kv.ModRevision > deleteRevision {
					deleteRevision = event.Kv.ModRevision
				}
			}
			if !deleted {
				continue
			}
			deletedLeader := watchedLeader
			if deleteRevision == 0 {
				deleteRevision = watchResponse.Header.Revision
			}
			if deleteRevision > deletedRevisionFloor {
				deletedRevisionFloor = deleteRevision
			}
			lastDeletedLeader = deletedLeader
			stopLeaderDeleteWatch()
			ownsLeadership = false

			currentLeader, leaderErr := readCurrentLeader()
			if leaderErr == nil && len(currentLeader.Kvs) == 0 {
				leaderErr = concurrency.ErrElectionNoLeader
			}
			switch {
			case leaderErr == nil:
				if !installLeaderResponseWatch(currentLeader) {
					generation.cancel()
					stopGenerationObservation()
					_ = stopCampaign()
					return generation.session.Lease(), ctx.Err() == nil
				}
			case errors.Is(leaderErr, concurrency.ErrElectionNoLeader):
				if campaignDone == nil {
					startCampaign()
				} else {
					recampaignPending = true
				}
				if !publish("") {
					generation.cancel()
					stopGenerationObservation()
					_ = stopCampaign()
					return 0, false
				}
			default:
				nextLease := reconnectLeaseAfterReadError()
				generation.cancel()
				stopGenerationObservation()
				_ = stopCampaign()
				if ctx.Err() == nil {
					logger.Zap.Error("while rechecking deleted election leader", zap.String("self", sd.server.ID),
						zap.String("key", deletedLeader.key), zap.Error(leaderErr))
				}
				return nextLease, ctx.Err() == nil
			}
		case <-ctx.Done():
			stopGenerationObservation()
			if campaignDone != nil {
				generation.cancel()
				if stopCampaign() == nil && !recampaignPending {
					ownsLeadership = true
				}
			}
			if ownsLeadership {
				resignCtx, cancel := context.WithTimeout(context.Background(), sd.electionShutdownTimeout())
				if err := generation.election.Resign(resignCtx); err != nil {
					logger.Zap.Error("while resigning leadership during shutdown", zap.String("self", sd.server.ID), zap.Error(err))
				}
				cancel()
			}
			generation.cancel()
			return 0, false
		case <-generation.session.Done():
			generation.cancel()
			stopGenerationObservation()
			_ = stopCampaign()
			return 0, ctx.Err() == nil
		}
	}
}

func (sd *etcdServiceDiscovery) newElectionGeneration(ctx context.Context, lease electionLease, token uint64) (*electionGeneration, error) {
	if lease.provenance == electionLeaseInvalid || (lease.id == 0 && lease.provenance != electionLeaseLocal) {
		return nil, errors.Errorf("invalid election lease request: id=%x provenance=%d", lease.id, lease.provenance)
	}
	generationCtx, cancel := context.WithCancel(ctx)
	generationClient := borrowEtcdClient(generationCtx, sd.cli)
	session, err := concurrency.NewSession(generationClient, concurrency.WithTTL(int(sd.lease.heartbeatTTL.Seconds())),
		concurrency.WithContext(generationCtx), concurrency.WithLease(lease.id))
	if err != nil {
		cancel()
		return nil, err
	}
	lease.id = session.Lease()
	return &electionGeneration{
		token:    token,
		ctx:      generationCtx,
		cancel:   cancel,
		session:  session,
		election: concurrency.NewElection(session, sd.election.name),
		lease:    lease,
	}, nil
}

func (sd *etcdServiceDiscovery) detachElectionGeneration(generation *electionGeneration) {
	generation.cancel()
	generation.session.Orphan()
}

func (sd *etcdServiceDiscovery) revokeElectionLease(lease electionLease, preserve bool) {
	if preserve || lease.id == 0 || lease.provenance != electionLeaseLocal {
		return
	}
	revokeCtx, cancel := context.WithTimeout(context.Background(), sd.electionShutdownTimeout())
	_, err := sd.cli.Revoke(revokeCtx, lease.id)
	cancel()
	if err != nil {
		logger.Zap.Error("while revoking detached election lease", zap.String("self", sd.server.ID),
			zap.Int64("lease", int64(lease.id)), zap.Error(err))
	}
}

func (sd *etcdServiceDiscovery) stopElectionGeneration(generation *electionGeneration, preserveLease bool) {
	lease := generation.lease
	sd.detachElectionGeneration(generation)
	sd.revokeElectionLease(lease, preserveLease)
}

func (sd *etcdServiceDiscovery) electionShutdownTimeout() time.Duration {
	if sd.lease.revokeTimeout > 0 && sd.lease.revokeTimeout < sd.lease.heartbeatTTL {
		return sd.lease.revokeTimeout
	}
	return sd.lease.heartbeatTTL
}

func (sd *etcdServiceDiscovery) LeaderID() string {
	sd.election.leaderMu.RLock()
	defer sd.election.leaderMu.RUnlock()
	return sd.election.leaderID
}
func (sd *etcdServiceDiscovery) IsLeader() bool {
	return sd.LeaderID() == sd.server.ID
}
