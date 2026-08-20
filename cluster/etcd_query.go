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

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

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

// GetServer returns a server given it's id
func (sd *etcdServiceDiscovery) GetServer(id string) (*Server, error) {
	if sv, ok := sd.watch.byID.Load(id); ok {
		return sv.(*Server), nil
	}
	return nil, errors.WithStack(fmt.Errorf("%w:%s", constants.ErrNoServerWithID, id))
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
