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

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/logger"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

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

func (sd *etcdServiceDiscovery) cancelElection() {
	sd.election.cancelMu.Lock()
	cancel := sd.election.cancel
	sd.election.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
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
