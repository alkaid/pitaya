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

	"github.com/pkg/errors"

	clientv3 "go.etcd.io/etcd/client/v3"
)

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
