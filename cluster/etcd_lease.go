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
	"fmt"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The large fixed capacity models an exclusive permit set; it is not a limit on
// the number of callers the service discovery API is intended to support.
const leasePublicationPermits int64 = 1 << 30

var errEtcdCandidateLeaseUnfenced = errors.New("etcd candidate lease could not be fenced")
var errEtcdServerIdentityOwned = errors.New("etcd server identity is already owned by another lease")

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
