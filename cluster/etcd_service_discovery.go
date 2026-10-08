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
	"reflect"
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
	"github.com/zeromicro/go-zero/core/hash"
	logutil "go.etcd.io/etcd/client/pkg/v3/logutil"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
	"google.golang.org/grpc"
)

type etcdServiceDiscovery struct {
	cli                  *clientv3.Client
	syncServersInterval  time.Duration
	heartbeatTTL         time.Duration
	logHeartbeat         bool
	lastHeartbeatTime    time.Time
	leaseID              clientv3.LeaseID
	mapByTypeLock        sync.RWMutex
	serverMapByType      map[string]map[string]*Server
	serverMapByID        sync.Map
	etcdEndpoints        []string
	etcdUser             string
	etcdPass             string
	etcdPrefix           string
	etcdDialTimeout      time.Duration
	running              atomic.Bool
	server               *Server
	stopChan             chan bool
	stopLeaseChan        chan bool
	lastSyncTime         time.Time
	listeners            []SDListener
	revokeTimeout        time.Duration
	grantLeaseTimeout    time.Duration
	grantLeaseMaxRetries int
	grantLeaseInterval   time.Duration
	shutdownDelay        time.Duration
	appDieChan           chan bool
	serverTypesBlacklist []string
	reconcileMu          sync.Mutex
	appliedRevision      int64
	ctx                  context.Context
	cancel               context.CancelFunc
	loops                sync.WaitGroup
	listenerMu           sync.RWMutex
	stopOnce             sync.Once
	consistentHash       map[string]*hash.ConsistentHash

	// 选主相关
	electionEnable   bool   // 是否开启选举
	electionName     string // 竞选名,不传的话默认为election/server.Type
	resumeLeader     bool   // 若连上后发现自己是leader 是否使用原leader创建election. false的话会辞职
	reconnectBackOff time.Duration
	session          *concurrency.Session
	election         *concurrency.Election
	electionCancel   context.CancelFunc // 选举取消
	leaderID         string
	setLeader        func(id string)
	onLeaderChange   LeaderChangeListener
}

// NewEtcdServiceDiscovery ctor
func NewEtcdServiceDiscovery(
	config config.EtcdServiceDiscoveryConfig,
	server *Server,
	appDieChan chan bool,
	cli ...*clientv3.Client,
) (ServiceDiscovery, error) {
	var client *clientv3.Client
	if len(cli) > 0 {
		client = cli[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	sd := &etcdServiceDiscovery{
		ctx: ctx, cancel: cancel,
		server:           server,
		serverMapByType:  make(map[string]map[string]*Server),
		listeners:        make([]SDListener, 0),
		stopChan:         make(chan bool),
		stopLeaseChan:    make(chan bool),
		appDieChan:       appDieChan,
		cli:              client,
		consistentHash:   make(map[string]*hash.ConsistentHash),
		resumeLeader:     true,
		reconnectBackOff: time.Second * 2,
		electionName:     server.Type,
		electionEnable:   false,
	}
	sd.configure(config)

	return sd, nil
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
	sd.electionEnable = config.Election.Enable
	if config.Election.Name != "" {
		sd.electionName = config.Election.Name
	}
	sd.electionName = "election/" + sd.electionName
}

func (sd *etcdServiceDiscovery) watchLeaseChan(c <-chan *clientv3.LeaseKeepAliveResponse) {
	failedGrantLeaseAttempts := 0
	for {
		select {
		case <-sd.stopChan:
			return
		case <-sd.stopLeaseChan:
			return
		case leaseKeepAliveResponse, ok := <-c:
			if !ok {
				logger.Zap.Error("ETCD lease KeepAlive died, retrying in 10 seconds")
				time.Sleep(10000 * time.Millisecond)
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
				err := sd.renewLease()
				if err != nil {
					failedGrantLeaseAttempts = failedGrantLeaseAttempts + 1
					if err == constants.ErrEtcdGrantLeaseTimeout {
						logger.Log.Warn("sd: timed out trying to grant etcd lease")
						if sd.appDieChan != nil {
							sd.appDieChan <- true
						}
						return
					}
					if failedGrantLeaseAttempts >= sd.grantLeaseMaxRetries {
						logger.Log.Warn("sd: exceeded max attempts to renew etcd lease")
						if sd.appDieChan != nil {
							sd.appDieChan <- true
						}
						return
					}
					logger.Log.Warnf("sd: error granting etcd lease, will retry in %d seconds", uint64(sd.grantLeaseInterval.Seconds()))
					time.Sleep(sd.grantLeaseInterval)
					continue
				}
				return
			}
		}
	}
}

// renewLease reestablishes connection with etcd
func (sd *etcdServiceDiscovery) renewLease() error {
	c := make(chan error, 1)
	co.Go(func() {
		defer close(c)
		logger.Zap.Info("waiting for etcd lease", zap.String("self", sd.server.ID))
		err := sd.grantLease()
		if err != nil {
			c <- err
			return
		}
		err = sd.bootstrapServer(sd.server)
		c <- err
	})
	select {
	case err := <-c:
		return err
	case <-time.After(sd.grantLeaseTimeout):
		return constants.ErrEtcdGrantLeaseTimeout
	}
}

func (sd *etcdServiceDiscovery) grantLease() error {
	// grab lease
	ctx, cancel := context.WithTimeout(context.Background(), sd.etcdDialTimeout)
	defer cancel()
	l, err := sd.cli.Grant(ctx, int64(sd.heartbeatTTL.Seconds()))
	if err != nil {
		return err
	}
	sd.leaseID = l.ID
	logger.Zap.Debug("sd: got leaseID", zap.Int64("leaseID", int64(l.ID)), zap.String("self", sd.server.ID))
	// this will keep alive forever, when channel c is closed
	// it means we probably have to rebootstrap the lease
	c, err := sd.cli.KeepAlive(context.TODO(), sd.leaseID)
	if err != nil {
		return err
	}
	// need to receive here as per etcd docs
	<-c
	co.Go(func() { sd.watchLeaseChan(c) })
	return nil
}

// FlushServer2Cluster
//
//	@implement ServiceDiscovery.FlushServer2Cluster
//	@receiver sd
//	@param server
//	@return error
func (sd *etcdServiceDiscovery) FlushServer2Cluster(server *Server) error {
	return sd.addServerIntoEtcd(server)
}

func (sd *etcdServiceDiscovery) addServerIntoEtcd(server *Server) error {
	_, err := sd.cli.Put(
		context.TODO(),
		getKey(server.ID, server.Type),
		server.AsJSONString(),
		clientv3.WithLease(sd.leaseID),
	)
	return err
}

func (sd *etcdServiceDiscovery) bootstrapServer(server *Server) error {
	if err := sd.addServerIntoEtcd(server); err != nil {
		return err
	}

	return sd.SyncServers(true)
}

// AddListener adds a listener to etcd service discovery
func (sd *etcdServiceDiscovery) AddListener(listener SDListener) {
	sd.listenerMu.Lock()
	defer sd.listenerMu.Unlock()
	sd.listeners = append(sd.listeners, listener)
}

// OnLeaderChange 设置leader变化监听
func (sd *etcdServiceDiscovery) OnLeaderChange(listener LeaderChangeListener) {
	sd.onLeaderChange = listener
}

// AfterInit executes after Init
func (sd *etcdServiceDiscovery) AfterInit() {
}

func (sd *etcdServiceDiscovery) notifyListeners(act Action, sv *Server, old ...*Server) {
	sd.listenerMu.RLock()
	listeners := append([]SDListener(nil), sd.listeners...)
	sd.listenerMu.RUnlock()
	for _, l := range listeners {
		if act == DEL {
			l.RemoveServer(sv)
		} else if act == ADD {
			l.AddServer(sv)
		} else if act == Modify {
			l.ModifyServer(sv, old[0])
		}
	}
}

func getKey(serverID, serverType string) string {
	return fmt.Sprintf("servers/%s/%s", serverType, serverID)
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
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	ha := sd.consistentHash[serverType]
	if ha == nil {
		return "", errors.WithStack(fmt.Errorf("%w server=%s,sid=%s", constants.ErrServerNotFound, serverType, sessionID))
	}
	node, ok := sd.consistentHash[serverType].Get(sessionID)
	if !ok {
		return "", errors.WithStack(fmt.Errorf("%w server=%s,sid=%s", constants.ErrServerNotFound, serverType, sessionID))
	}
	return node.(string), nil
}

// GetServers returns a slice with all the servers
func (sd *etcdServiceDiscovery) GetServers() []*Server {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	ret := make([]*Server, 0)
	sd.serverMapByID.Range(func(k, v interface{}) bool {
		ret = append(ret, v.(*Server))
		return true
	})
	return ret
}

func (sd *etcdServiceDiscovery) GetAnyFrontend() (*Server, error) {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
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
	if err := sd.grantLease(); err != nil {
		return err
	}

	// 选举
	if sd.electionEnable {
		var ctx context.Context
		ctx, sd.electionCancel = context.WithCancel(context.Background())
		leaderChan, err := sd.runElection(ctx)
		if err != nil {
			return err
		}
		co.Go(func() {
			for leader := range leaderChan {
				sd.leaderID = leader
				if sd.onLeaderChange != nil {
					sd.onLeaderChange(leader)
				}
			}
		})

	}

	if err := sd.bootstrapServer(sd.server); err != nil {
		return err
	}

	return nil
}

// GetServer returns a server given it's id
func (sd *etcdServiceDiscovery) GetServer(id string) (*Server, error) {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	if sv, ok := sd.serverMapByID.Load(id); ok {
		return sv.(*Server), nil
	}
	return nil, errors.WithStack(fmt.Errorf("%w:%s", constants.ErrNoServerWithID, id))
}

func (sd *etcdServiceDiscovery) InitETCDClient() error {
	logger.Zap.Info("Initializing ETCD client")
	var cli *clientv3.Client
	var err error
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
	cli, err = clientv3.New(config)
	if err != nil {
		logger.Zap.Error("error initializing etcd client", zap.Error(err))
		return err
	}
	sd.cli = cli

	// namespaced etcd :)
	sd.cli.KV = namespace.NewKV(sd.cli.KV, sd.etcdPrefix)
	sd.cli.Watcher = namespace.NewWatcher(sd.cli.Watcher, sd.etcdPrefix)
	sd.cli.Lease = namespace.NewLease(sd.cli.Lease, sd.etcdPrefix)
	return nil
}

// Init starts the service discovery client
func (sd *etcdServiceDiscovery) Init() error {
	sd.running.Store(true)
	var err error

	if sd.cli == nil {
		err = sd.InitETCDClient()
		if err != nil {
			return err
		}
	} else {
		sd.cli.KV = namespace.NewKV(sd.cli.KV, sd.etcdPrefix)
		sd.cli.Watcher = namespace.NewWatcher(sd.cli.Watcher, sd.etcdPrefix)
		sd.cli.Lease = namespace.NewLease(sd.cli.Lease, sd.etcdPrefix)
	}

	if err = sd.bootstrap(); err != nil {
		sd.running.Store(false)
		sd.cancel()
		return err
	}
	sd.loops.Go(sd.watchEtcdChanges)

	// update servers
	syncServersTicker := time.NewTicker(sd.syncServersInterval)
	sd.loops.Go(func() {
		defer syncServersTicker.Stop()
		for sd.running.Load() {
			select {
			case <-syncServersTicker.C:
				err := sd.SyncServers(false)
				if err != nil {
					logger.Zap.Error("error resyncing servers", zap.Error(err))
				}
			case <-sd.stopChan:
				return
			}
		}
	})

	return nil
}

func parseEtcdKey(key string) (string, string, error) {
	splittedServer := strings.Split(key, "/")
	if len(splittedServer) != 3 || splittedServer[0] != "servers" || splittedServer[1] == "" || splittedServer[2] == "" {
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
	if sv == nil || sv.ID == "" || sv.Type == "" {
		return nil, fmt.Errorf("invalid server registration")
	}
	sv.fix()
	return sv, nil
}

func (sd *etcdServiceDiscovery) printServers() {
	sd.mapByTypeLock.RLock()
	defer sd.mapByTypeLock.RUnlock()
	for k, v := range sd.serverMapByType {
		logger.Zap.Debug("", zap.String("self", sd.server.ID), zap.String("type", k), zap.Any("servers", v))
	}
}

// SyncServers reads one consistent prefix snapshot. firstSync is retained for API compatibility.
// Notifications complete before this method returns; listeners must not call it synchronously.
func (sd *etcdServiceDiscovery) SyncServers(firstSync bool) error {
	sd.reconcileMu.Lock()
	defer sd.reconcileMu.Unlock()
	return sd.syncServersLocked()
}

func (sd *etcdServiceDiscovery) syncServersLocked() error {
	ctx, cancel := context.WithTimeout(sd.ctx, sd.etcdDialTimeout)
	defer cancel()
	snapshot, err := sd.cli.Get(ctx, "servers/", clientv3.WithPrefix())
	if err != nil {
		return err
	}
	if err = sd.ctx.Err(); err != nil {
		return err
	}
	if snapshot.Header.Revision < sd.appliedRevision {
		return nil
	}
	servers := make(map[string]*Server, len(snapshot.Kvs))
	for _, kv := range snapshot.Kvs {
		typ, id, err := parseEtcdKey(string(kv.Key))
		if err != nil {
			logger.Zap.Warn("invalid etcd registration key", zap.Error(err))
			continue
		}
		if sd.isServerTypeBlacklisted(typ) && id != sd.server.ID {
			continue
		}
		sv, err := parseServer(kv.Value)
		if err == nil && (sv.ID != id || sv.Type != typ) {
			err = fmt.Errorf("registration does not match key %s", kv.Key)
		}
		if err != nil {
			logger.Zap.Warn("invalid etcd registration; retaining last valid value", zap.String("key", string(kv.Key)), zap.Error(err))
			if old, ok := sd.serverMapByID.Load(id); ok {
				servers[id] = old.(*Server)
			}
			continue
		}
		servers[id] = sv
	}
	changes := make([]serverChange, 0)
	sd.mapByTypeLock.Lock()
	for id, sv := range servers {
		old, ok := sd.serverMapByID.Load(id)
		if !ok || !reflect.DeepEqual(old, sv) {
			changes = append(changes, sd.storeServerLocked(sv))
		}
	}
	sd.serverMapByID.Range(func(key, value interface{}) bool {
		if _, ok := servers[key.(string)]; !ok {
			changes = append(changes, sd.removeServerLocked(value.(*Server)))
		}
		return true
	})
	sd.mapByTypeLock.Unlock()
	sd.appliedRevision = snapshot.Header.Revision
	sd.lastSyncTime = time.Now()
	sd.notifyChanges(changes)
	return nil
}

type serverChange struct {
	action      Action
	server, old *Server
}

// The reconcile mutex owns revisions and notifications; cache locks never surround application callbacks.
func (sd *etcdServiceDiscovery) notifyChanges(changes []serverChange) {
	for _, change := range changes {
		if change.action == DEL || change.server.ID != sd.server.ID {
			sd.notifyListeners(change.action, change.server, change.old)
		}
	}
}

func (sd *etcdServiceDiscovery) storeServerLocked(sv *Server) serverChange {
	old, loaded := sd.serverMapByID.Load(sv.ID)
	change := serverChange{action: ADD, server: sv}
	if loaded {
		previous := old.(*Server)
		change.action = Modify
		change.old = previous
		if previous.Type != sv.Type {
			sd.removeServerLocked(previous)
		}
	}
	sd.serverMapByID.Store(sv.ID, sv)
	if sd.serverMapByType[sv.Type] == nil {
		sd.serverMapByType[sv.Type] = make(map[string]*Server)
	}
	sd.serverMapByType[sv.Type][sv.ID] = sv
	if sv.ID != sd.server.ID {
		if sd.consistentHash[sv.Type] == nil {
			sd.consistentHash[sv.Type] = hash.NewConsistentHash()
		}
		sd.consistentHash[sv.Type].Add(sv.ID)
	}
	return change
}

func (sd *etcdServiceDiscovery) removeServerLocked(sv *Server) serverChange {
	sd.serverMapByID.Delete(sv.ID)
	delete(sd.serverMapByType[sv.Type], sv.ID)
	if h := sd.consistentHash[sv.Type]; h != nil {
		h.Remove(sv.ID)
	}
	return serverChange{action: DEL, server: sv}
}

func (sd *etcdServiceDiscovery) applyWatchResponse(response clientv3.WatchResponse) {
	sd.reconcileMu.Lock()
	defer sd.reconcileMu.Unlock()
	if sd.ctx.Err() != nil {
		return
	}
	boundary := sd.appliedRevision
	revision := boundary
	changes := make([]serverChange, 0, len(response.Events))
	sd.mapByTypeLock.Lock()
	for _, event := range response.Events {
		// Compare every event against the previous batch, not its sibling events.
		if event.Kv.ModRevision <= boundary {
			continue
		}
		if event.Kv.ModRevision > revision {
			revision = event.Kv.ModRevision
		}
		typ, id, err := parseEtcdKey(string(event.Kv.Key))
		if err != nil || (sd.isServerTypeBlacklisted(typ) && id != sd.server.ID) {
			continue
		}
		switch event.Type {
		case clientv3.EventTypePut:
			sv, err := parseServer(event.Kv.Value)
			if err != nil || sv.ID != id || sv.Type != typ {
				logger.Zap.Warn("invalid watched registration", zap.String("key", string(event.Kv.Key)), zap.Error(err))
				continue
			}
			changes = append(changes, sd.storeServerLocked(sv))
		case clientv3.EventTypeDelete:
			if old, ok := sd.serverMapByID.Load(id); ok {
				changes = append(changes, sd.removeServerLocked(old.(*Server)))
			}
		}
	}
	sd.mapByTypeLock.Unlock()
	// Created/progress headers can be ahead of events still queued for delivery.
	sd.appliedRevision = revision
	sd.notifyChanges(changes)
}

func (sd *etcdServiceDiscovery) Running() bool {
	return sd.running.Load()
}

// BeforeShutdown executes before shutting down and will remove the server from the list
func (sd *etcdServiceDiscovery) BeforeShutdown() {
	sd.listenerMu.Lock()
	sd.listeners = nil
	sd.listenerMu.Unlock()
	sd.revoke()
	if sd.electionCancel != nil {
		sd.electionCancel()
	}
	time.Sleep(sd.shutdownDelay) // Sleep for a short while to ensure shutdown has propagated
}

// Shutdown executes on shutdown and will clean etcd
func (sd *etcdServiceDiscovery) Shutdown() error {
	sd.stopOnce.Do(func() {
		sd.running.Store(false)
		sd.cancel()
		close(sd.stopChan)
	})
	sd.loops.Wait()
	// Public SyncServers calls are not owned by loops. Join their notifications too.
	sd.reconcileMu.Lock()
	sd.reconcileMu.Unlock()
	return nil
}

// revoke prevents Pitaya from crashing when etcd is not available
func (sd *etcdServiceDiscovery) revoke() error {
	close(sd.stopLeaseChan)
	c := make(chan error, 1)
	co.Go(func() {
		defer close(c)
		logger.Zap.Debug("waiting for etcd revoke", zap.String("self", sd.server.ID))
		_, err := sd.cli.Revoke(context.TODO(), sd.leaseID)
		c <- err
		logger.Zap.Debug("finished waiting for etcd revoke", zap.String("self", sd.server.ID))
	})
	select {
	case err := <-c:
		return err // completed normally
	case <-time.After(sd.revokeTimeout):
		logger.Zap.Warn("timed out waiting for etcd revoke", zap.String("self", sd.server.ID))
		return nil // timed out
	}
}

func (sd *etcdServiceDiscovery) watchEtcdChanges() {
	for sd.ctx.Err() == nil {
		sd.reconcileMu.Lock()
		err := sd.syncServersLocked()
		revision := sd.appliedRevision
		sd.reconcileMu.Unlock()
		if err == nil {
			ctx, cancel := context.WithCancel(sd.ctx)
			watch := sd.cli.Watch(ctx, "servers/", clientv3.WithPrefix(), clientv3.WithRev(revision+1))
		consume:
			for {
				select {
				case response, ok := <-watch:
					if !ok || response.Err() != nil {
						break consume
					}
					sd.applyWatchResponse(response)
				case <-ctx.Done():
					break consume
				}
			}
			cancel()
		} else if sd.ctx.Err() == nil {
			logger.Zap.Warn("etcd snapshot failed; retrying", zap.Error(err))
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-sd.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (sd *etcdServiceDiscovery) isServerTypeBlacklisted(svType string) bool {
	for _, blacklistedSv := range sd.serverTypesBlacklist {
		if blacklistedSv == svType {
			return true
		}
	}
	return false
}

func (sd *etcdServiceDiscovery) watchLeader(ctx context.Context) {
	w := sd.cli.Watch(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
	failedWatchAttempts := 0
	go func(chn clientv3.WatchChan) {
		for sd.running.Load() {
			select {
			case wResp, ok := <-chn:
				if wResp.Err() != nil {
					logger.Zap.Warn("etcd watcher response error", zap.String("self", sd.server.ID), zap.Error(wResp.Err()))
					time.Sleep(100 * time.Millisecond)
				}
				if !ok {
					logger.Log.Error("etcd watcher died, retrying to watch in 1 second")
					failedWatchAttempts++
					time.Sleep(1000 * time.Millisecond)
					if failedWatchAttempts > 10 {
						// 这里可能和 watchEtcdChanges()冲突 产生多个client
						if err := sd.InitETCDClient(); err != nil {
							failedWatchAttempts = 0
							continue
						}
						chn = sd.cli.Watch(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
						failedWatchAttempts = 0
					}
					continue
				}
				failedWatchAttempts = 0
				resp, err := sd.election.Leader(ctx)
				if err != nil {
					logger.Log.Error("etcd election error", zap.String("self", sd.server.ID), zap.Error(err))
					time.Sleep(time.Millisecond * 300)
					continue
				}
				logger.Zap.Debug("etcd election watcher", zap.String("self", sd.server.ID), zap.ByteString("leader", resp.Kvs[0].Value))
				// 当竞选成功时这里会和竞选函数里重复调用，不影响结果故可忽略
				if sd.setLeader != nil {
					sd.setLeader(string(resp.Kvs[0].Value))
				}
			case <-ctx.Done():
				return
			case <-sd.stopChan:
				return
			}
		}
	}(w)
}

func (sd *etcdServiceDiscovery) runElection(ctx context.Context) (<-chan string, error) {
	var observe <-chan clientv3.GetResponse
	var node *clientv3.GetResponse
	var errChan chan error
	var leaderID string
	var err error

	var leaderChan chan string
	setLeader := func(id string) {
		// Only report changes in leadership
		if leaderID == id {
			logger.Zap.Debug("etcd election leader remains the same,ignore", zap.String("leaderID", leaderID), zap.String("self", sd.server.ID))
			return
		}
		logger.Zap.Info("etcd election leader changed", zap.String("name", sd.electionName),
			zap.String("leaderID", id), zap.String("old", leaderID), zap.String("self", sd.server.ID))
		leaderID = id
		leaderChan <- id
	}
	sd.setLeader = setLeader

	if err = sd.newSession(ctx, sd.leaseID); err != nil {
		return nil, errors.Wrap(err, "while creating initial session")
	}

	leaderChan = make(chan string, 10)
	go func() {
		defer close(leaderChan)

		for {
			// Discover who if any, is leader of this election
			if node, err = sd.election.Leader(ctx); err != nil {
				if err != concurrency.ErrElectionNoLeader {
					logger.Zap.Error("while determining election leader", zap.String("self", sd.server.ID), zap.Error(err))
					goto reconnect
				}
			} else {
				// If we are resuming an election from which we previously had leadership we
				// have 2 options
				// 1. Resume the leadership if the lease has not expired. This is a race as the
				//    lease could expire in between the `Leader()` call and when we resume
				//    observing changes to the election. If this happens we should detect the
				//    session has expired during the observation loop.
				// 2. Resign the leadership immediately to allow a new leader to be chosen.
				//    This option will almost always result in transfer of leadership.
				if string(node.Kvs[0].Value) == sd.server.ID {
					// If we want to resume leadership
					if sd.resumeLeader {
						// Recreate our session with the old lease id
						if err = sd.newSession(ctx, clientv3.LeaseID(node.Kvs[0].Lease)); err != nil {
							logger.Zap.Error("while re-establishing session with lease", zap.String("self", sd.server.ID), zap.Error(err))
							goto reconnect
						}
						sd.election = concurrency.ResumeElection(sd.session, sd.electionName,
							string(node.Kvs[0].Key), node.Kvs[0].CreateRevision)

						// Because Campaign() only returns if the election entry doesn't exist
						// we must skip the campaign call and go directly to observe when resuming
						goto observe
					} else {
						// If resign takes longer than our TTL then lease is expired and we are no
						// longer leader anyway.
						ctx, cancel := context.WithTimeout(ctx, sd.heartbeatTTL)
						election := concurrency.ResumeElection(sd.session, sd.electionName,
							string(node.Kvs[0].Key), node.Kvs[0].CreateRevision)
						err = election.Resign(ctx)
						cancel()
						if err != nil {
							logger.Zap.Error("while resigning leadership after reconnect", zap.String("self", sd.server.ID), zap.Error(err))
							goto reconnect
						}
					}
				}
			}
			// Reset leadership if we had it previously
			if err != nil {
				// no leader
				setLeader("")
			} else {
				setLeader(string(node.Kvs[0].Value))
			}

			// Attempt to become leader
			errChan = make(chan error)
			go func() {
				// Make this a non blocking call so we can check for session close
				errChan <- sd.election.Campaign(ctx, sd.server.ID)
			}()

			select {
			case err = <-errChan:
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					// NOTE: Campaign currently does not return an error if session expires
					logger.Zap.Error("while campaigning for leader", zap.String("self", sd.server.ID), zap.Error(err))
					sd.session.Close()
					goto reconnect
				}
			case <-ctx.Done():
				sd.session.Close()
				return
			case <-sd.session.Done():
				goto reconnect
			}

		observe:
			// If Campaign() returned without error, we are leader
			setLeader(sd.server.ID)

			// Observe changes to leadership
			observe = sd.election.Observe(ctx)
			for {
				select {
				case resp, ok := <-observe:
					if !ok {
						// NOTE: Observe will not close if the session expires, we must
						// watch for session.Done()
						logger.Zap.Error("election observe stream closed", zap.String("self", sd.server.ID))
						sd.session.Close()
						goto reconnect
					}
					logger.Zap.Debug("etcd election observe", zap.String("self", sd.server.ID), zap.ByteString("leader", resp.Kvs[0].Value))
					if string(resp.Kvs[0].Value) == sd.server.ID {
						setLeader(string(resp.Kvs[0].Value))
					} else {
						// We are not leader
						setLeader(string(resp.Kvs[0].Value))
						break
					}
				case <-ctx.Done():
					if leaderID == sd.server.ID {
						// If resign takes longer than our TTL then lease is expired and we are no
						// longer leader anyway.
						ctx, cancel := context.WithTimeout(context.Background(), sd.heartbeatTTL)
						if err = sd.election.Resign(ctx); err != nil {
							logger.Zap.Error("while resigning leadership during shutdown", zap.String("self", sd.server.ID), zap.Error(err))
						}
						cancel()
					}
					sd.session.Close()
					return
				case <-sd.session.Done():
					goto reconnect
				}
			}

		reconnect:
			logger.Zap.Debug("etcd election need reconnect", zap.String("self", sd.server.ID))
			// 出错重连 无主
			setLeader("")
			for {
				if err = sd.newSession(ctx, 0); err != nil {
					if errors.Cause(err) == context.Canceled {
						return
					}
					logger.Zap.Error("while creating new session", zap.String("self", sd.server.ID), zap.Error(err))
					tick := time.NewTicker(sd.reconnectBackOff)
					select {
					case <-ctx.Done():
						tick.Stop()
						return
					case <-tick.C:
						tick.Stop()
					}
					continue
				}
				break
			}
		}
	}()

	// 非leader成员监听leader变化
	go sd.watchLeader(ctx)

	// Wait until we have a leader before returning
	for {
		resp, err := sd.election.Leader(ctx)
		if err != nil {
			if err != concurrency.ErrElectionNoLeader {
				return nil, err
			}
			time.Sleep(time.Millisecond * 300)
			continue
		}
		// If we are not leader, notify the channel
		logger.Zap.Debug("init leader", zap.String("self", sd.server.ID), zap.ByteString("leader", resp.Kvs[0].Value))
		setLeader(string(resp.Kvs[0].Value))
		break
	}
	return leaderChan, nil
}

func (sd *etcdServiceDiscovery) newSession(ctx context.Context, id clientv3.LeaseID) error {
	var err error
	sd.session, err = concurrency.NewSession(sd.cli, concurrency.WithTTL(int(sd.heartbeatTTL.Seconds())),
		concurrency.WithContext(ctx), concurrency.WithLease(id))
	if err != nil {
		return err
	}
	sd.election = concurrency.NewElection(sd.session, sd.electionName)
	return nil
}

func (sd *etcdServiceDiscovery) LeaderID() string {
	return sd.leaderID
}
func (sd *etcdServiceDiscovery) IsLeader() bool {
	return sd.leaderID == sd.server.ID
}
