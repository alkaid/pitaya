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
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/zeromicro/go-zero/core/hash"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sync/semaphore"
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
