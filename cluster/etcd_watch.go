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
	"github.com/zeromicro/go-zero/core/hash"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// AddListener adds a listener to etcd service discovery
func (sd *etcdServiceDiscovery) AddListener(listener SDListener) {
	sd.watch.listenersMu.Lock()
	defer sd.watch.listenersMu.Unlock()
	sd.watch.listeners = append(sd.watch.listeners, listener)
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
