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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

var etcdSDTables = []struct {
	server *Server
}{
	{NewServer("frontend-1", "type1", true, map[string]string{"k1": "v1"})},
	{NewServer("backend-1", "type2", false, map[string]string{"k2": "v2"})},
	{NewServer("backend-2", "type3", false, map[string]string{})},
}

var etcdSDTablesMultipleServers = []struct {
	name    string
	servers []*Server
}{
	{"empty", []*Server{}},
	{"multiple-types", []*Server{
		NewServer("frontend-1", "type1", true, map[string]string{"k1": "v1"}),
		NewServer("backend-1", "type2", false, map[string]string{"k2": "v2"}),
		NewServer("backend-2", "type3", false, map[string]string{}),
	}},
	{"same-type", []*Server{
		NewServer("frontend-1", "type2", true, map[string]string{"k1": "v1"}),
		NewServer("frontend-2", "type2", true, map[string]string{"k2": "v2"}),
		NewServer("frontend-3", "type2", true, map[string]string{"k1": "v1"}),
	}},
}

var etcdSDBlacklistTables = []struct {
	name                string
	server              *Server
	serversToAdd        []*Server
	serverTypeBlacklist []string
}{
	{
		name:   "test1",
		server: NewServer("frontend-1", "type1", true, nil),
		serversToAdd: []*Server{
			NewServer("frontend-1", "type1", true, nil),
		},
		serverTypeBlacklist: nil,
	},
	{
		name:   "test2",
		server: NewServer("frontend-1", "type1", true, nil),
		serversToAdd: []*Server{
			NewServer("backend-1", "type1", false, nil),
			NewServer("backend-2", "type2", false, nil),
			NewServer("backend-3", "type3", false, nil),
		},
		serverTypeBlacklist: []string{"type2"},
	},
	{
		name:   "test3",
		server: NewServer("frontend-1", "type1", true, nil),
		serversToAdd: []*Server{
			NewServer("backend-1", "type1", false, nil),
			NewServer("backend-2", "type2", false, nil),
			NewServer("backend-3", "type3", false, nil),
			NewServer("backend-4", "type4", false, nil),
		},
		serverTypeBlacklist: []string{"type1", "type4"},
	},
}

type testSDListener struct {
	add    func(*Server)
	remove func(*Server)
	modify func(*Server, *Server)
}

func (l *testSDListener) AddServer(server *Server) {
	if l.add != nil {
		l.add(server)
	}
}

func (l *testSDListener) RemoveServer(server *Server) {
	if l.remove != nil {
		l.remove(server)
	}
}

func (l *testSDListener) ModifyServer(server, old *Server) {
	if l.modify != nil {
		l.modify(server, old)
	}
}

type failingGetKV struct {
	clientv3.KV
	err error
}

type failingPutKV struct {
	clientv3.KV
	err error
}

type failFirstGetKV struct {
	clientv3.KV
	lock  sync.Mutex
	calls int
	err   error
}

type blockElectionGetKV struct {
	clientv3.KV
	lock                  sync.Mutex
	getCalls              int
	blockAt               int
	controlCtx            context.Context
	txnCalls              int
	firstCampaignCtx      context.Context
	started               chan struct{}
	release               chan struct{}
	firstCampaignCanceled chan struct{}
	secondTxnStarted      chan struct{}
}

type blockBoundedElectionGetKV struct {
	clientv3.KV
	lock    sync.Mutex
	calls   int
	blockAt int
	started chan struct{}
	release chan struct{}
}

func (kv *blockBoundedElectionGetKV) Get(
	ctx context.Context,
	key string,
	opts ...clientv3.OpOption,
) (*clientv3.GetResponse, error) {
	_, bounded := ctx.Deadline()
	kv.lock.Lock()
	if bounded && strings.Contains(key, "election/") {
		kv.calls++
	}
	call := kv.calls
	kv.lock.Unlock()
	if bounded && call == kv.blockAt {
		close(kv.started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-kv.release:
		}
	}
	return kv.KV.Get(ctx, key, opts...)
}

func (kv *blockElectionGetKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	if strings.Contains(key, "election/") {
		_, bounded := ctx.Deadline()
		kv.lock.Lock()
		if bounded || ctx == kv.controlCtx || (kv.firstCampaignCtx != nil && ctx == kv.firstCampaignCtx) {
			kv.lock.Unlock()
			return kv.KV.Get(ctx, key, opts...)
		}
		kv.getCalls++
		call := kv.getCalls
		kv.lock.Unlock()
		if call == kv.blockAt {
			close(kv.started)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-kv.release:
			}
		}
	}
	return kv.KV.Get(ctx, key, opts...)
}

func (kv *blockElectionGetKV) Txn(ctx context.Context) clientv3.Txn {
	kv.lock.Lock()
	kv.txnCalls++
	call := kv.txnCalls
	if call == 1 {
		kv.firstCampaignCtx = ctx
	}
	kv.lock.Unlock()
	if call == 1 && kv.firstCampaignCanceled != nil {
		go func() {
			<-ctx.Done()
			close(kv.firstCampaignCanceled)
		}()
	}
	if call == 2 && kv.secondTxnStarted != nil {
		close(kv.secondTxnStarted)
	}
	return kv.KV.Txn(ctx)
}

func (kv *failFirstGetKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	kv.lock.Lock()
	kv.calls++
	call := kv.calls
	kv.lock.Unlock()
	if call == 1 {
		return nil, kv.err
	}
	return kv.KV.Get(ctx, key, opts...)
}

type postCommitTimeoutPutKV struct {
	clientv3.KV
	returnError bool
}

type postCommitTimeoutTxn struct {
	ctx         context.Context
	kv          clientv3.KV
	returnError bool
	cmps        []clientv3.Cmp
	thenOps     []clientv3.Op
	elseOps     []clientv3.Op
}

type commitHookTxn struct {
	clientv3.Txn
	commit func(clientv3.Txn) (*clientv3.TxnResponse, error)
}

type deferredMutationResult struct {
	txnResponse *clientv3.TxnResponse
	putResponse *clientv3.PutResponse
	err         error
}

type deferredMutationKV struct {
	clientv3.KV
	err     error
	started chan struct{}
	release chan struct{}
	result  chan deferredMutationResult
	once    sync.Once
	lock    sync.Mutex
	calls   int
}

type deferredCommitTxn struct {
	kv      *deferredMutationKV
	cmps    []clientv3.Cmp
	thenOps []clientv3.Op
	elseOps []clientv3.Op
}

func (txn *deferredCommitTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.cmps = append(txn.cmps, cmps...)
	return txn
}

func (txn *deferredCommitTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	txn.thenOps = append(txn.thenOps, ops...)
	return txn
}

func (txn *deferredCommitTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.elseOps = append(txn.elseOps, ops...)
	return txn
}

func (txn *deferredCommitTxn) Commit() (*clientv3.TxnResponse, error) {
	txn.kv.once.Do(func() {
		close(txn.kv.started)
		go func() {
			<-txn.kv.release
			actual := txn.kv.KV.Txn(context.Background()).If(txn.cmps...).Then(txn.thenOps...)
			if len(txn.elseOps) > 0 {
				actual = actual.Else(txn.elseOps...)
			}
			response, err := actual.Commit()
			txn.kv.result <- deferredMutationResult{txnResponse: response, err: err}
		}()
	})
	return nil, txn.kv.err
}

func (kv *deferredMutationKV) shouldDefer() bool {
	kv.lock.Lock()
	defer kv.lock.Unlock()
	kv.calls++
	return kv.calls == 1
}

func (kv *deferredMutationKV) Txn(ctx context.Context) clientv3.Txn {
	if !kv.shouldDefer() {
		return kv.KV.Txn(ctx)
	}
	return &deferredCommitTxn{kv: kv}
}

func (kv *deferredMutationKV) Put(ctx context.Context, key, value string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	if !kv.shouldDefer() {
		return kv.KV.Put(ctx, key, value, opts...)
	}
	capturedOpts := append([]clientv3.OpOption(nil), opts...)
	kv.once.Do(func() {
		close(kv.started)
		go func() {
			<-kv.release
			response, err := kv.KV.Put(context.Background(), key, value, capturedOpts...)
			kv.result <- deferredMutationResult{putResponse: response, err: err}
		}()
	})
	return nil, kv.err
}

func (txn *commitHookTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.Txn = txn.Txn.If(cmps...)
	return txn
}

func (txn *commitHookTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Then(ops...)
	return txn
}

func (txn *commitHookTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(ops...)
	return txn
}

func (txn *commitHookTxn) Commit() (*clientv3.TxnResponse, error) {
	return txn.commit(txn.Txn)
}

func (kv *postCommitTimeoutPutKV) Put(ctx context.Context, key, value string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	response, err := kv.KV.Put(context.WithoutCancel(ctx), key, value, opts...)
	if err != nil {
		return response, err
	}
	<-ctx.Done()
	if kv.returnError {
		return response, ctx.Err()
	}
	return response, nil
}

func (kv *postCommitTimeoutPutKV) Txn(ctx context.Context) clientv3.Txn {
	return &postCommitTimeoutTxn{
		ctx:         ctx,
		kv:          kv.KV,
		returnError: kv.returnError,
	}
}

func (txn *postCommitTimeoutTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.cmps = append(txn.cmps, cmps...)
	return txn
}

func (txn *postCommitTimeoutTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	txn.thenOps = append(txn.thenOps, ops...)
	return txn
}

func (txn *postCommitTimeoutTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.elseOps = append(txn.elseOps, ops...)
	return txn
}

func (txn *postCommitTimeoutTxn) Commit() (*clientv3.TxnResponse, error) {
	actual := txn.kv.Txn(context.WithoutCancel(txn.ctx)).If(txn.cmps...).Then(txn.thenOps...)
	if len(txn.elseOps) > 0 {
		actual = actual.Else(txn.elseOps...)
	}
	response, err := actual.Commit()
	if err != nil {
		return response, err
	}
	<-txn.ctx.Done()
	if txn.returnError {
		return response, txn.ctx.Err()
	}
	return response, nil
}

type concurrentBlockingPutKV struct {
	clientv3.KV
	lock          sync.Mutex
	putCalls      int
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
}

func (kv *concurrentBlockingPutKV) nextCall() int {
	kv.lock.Lock()
	defer kv.lock.Unlock()
	kv.putCalls++
	return kv.putCalls
}

func (kv *concurrentBlockingPutKV) Put(ctx context.Context, key, value string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	call := kv.nextCall()
	switch call {
	case 1:
		close(kv.firstStarted)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-kv.releaseFirst:
		}
	case 2:
		close(kv.secondStarted)
	}
	return kv.KV.Put(ctx, key, value, opts...)
}

func (kv *concurrentBlockingPutKV) Txn(ctx context.Context) clientv3.Txn {
	return &commitHookTxn{
		Txn: kv.KV.Txn(ctx),
		commit: func(txn clientv3.Txn) (*clientv3.TxnResponse, error) {
			call := kv.nextCall()
			switch call {
			case 1:
				close(kv.firstStarted)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-kv.releaseFirst:
				}
			case 2:
				close(kv.secondStarted)
			}
			return txn.Commit()
		},
	}
}

type postCommitBlockingPutKV struct {
	clientv3.KV
	lock           sync.Mutex
	putCalls       int
	firstCommitted chan struct{}
	secondStarted  chan struct{}
	releaseFirst   chan struct{}
}

func (kv *postCommitBlockingPutKV) nextCall() int {
	kv.lock.Lock()
	defer kv.lock.Unlock()
	kv.putCalls++
	return kv.putCalls
}

func (kv *postCommitBlockingPutKV) Put(ctx context.Context, key, value string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	call := kv.nextCall()
	if call == 1 {
		response, err := kv.KV.Put(ctx, key, value, opts...)
		if err != nil {
			return response, err
		}
		close(kv.firstCommitted)
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		case <-kv.releaseFirst:
			return response, nil
		}
	}
	if call == 2 {
		close(kv.secondStarted)
	}
	return kv.KV.Put(ctx, key, value, opts...)
}

func (kv *postCommitBlockingPutKV) Txn(ctx context.Context) clientv3.Txn {
	return &commitHookTxn{
		Txn: kv.KV.Txn(ctx),
		commit: func(txn clientv3.Txn) (*clientv3.TxnResponse, error) {
			call := kv.nextCall()
			if call == 1 {
				response, err := txn.Commit()
				if err != nil {
					return response, err
				}
				close(kv.firstCommitted)
				select {
				case <-ctx.Done():
					return response, ctx.Err()
				case <-kv.releaseFirst:
					return response, nil
				}
			}
			if call == 2 {
				close(kv.secondStarted)
			}
			return txn.Commit()
		},
	}
}

type firstPutBlockingKV struct {
	clientv3.KV
	lock         sync.Mutex
	putCalls     int
	firstStarted chan struct{}
	releaseFirst chan struct{}
	deadline     chan bool
}

func (kv *firstPutBlockingKV) nextCall() int {
	kv.lock.Lock()
	defer kv.lock.Unlock()
	kv.putCalls++
	return kv.putCalls
}

type invalidFirstWatcher struct {
	clientv3.Watcher
	lock     sync.Mutex
	calls    int
	response clientv3.WatchResponse
}

type closeFirstElectionWatcher struct {
	clientv3.Watcher
	lock    sync.Mutex
	calls   int
	closeAt int
	done    chan struct{}
}

type delayedElectionObserveWatcher struct {
	clientv3.Watcher
	lock                    sync.Mutex
	leaderKey               string
	leaderKeyCalls          int
	replacementKey          string
	successorKey            string
	observeOnce             sync.Once
	replacementOnce         sync.Once
	successorOnce           sync.Once
	observeStarted          chan struct{}
	replacementWatchStarted chan struct{}
	successorWatchStarted   chan struct{}
	observeResponses        <-chan clientv3.WatchResponse
}

func (watcher *delayedElectionObserveWatcher) Watch(
	ctx context.Context,
	key string,
	opts ...clientv3.OpOption,
) clientv3.WatchChan {
	if !strings.Contains(key, "election/") {
		return watcher.Watcher.Watch(ctx, key, opts...)
	}
	watcher.lock.Lock()
	if key == watcher.leaderKey {
		watcher.leaderKeyCalls++
	}
	leaderKeyCall := watcher.leaderKeyCalls
	watcher.lock.Unlock()
	switch {
	case key == watcher.leaderKey && leaderKeyCall == 2:
		// Observe cannot install this watch until its unbuffered initial snapshot
		// is consumed, after the production delete watch has claimed call one.
		watcher.observeOnce.Do(func() { close(watcher.observeStarted) })
		responses := make(chan clientv3.WatchResponse)
		go func() {
			defer close(responses)
			for {
				select {
				case response, ok := <-watcher.observeResponses:
					if !ok {
						return
					}
					select {
					case responses <- response:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
		return responses
	case key == watcher.replacementKey:
		watcher.replacementOnce.Do(func() { close(watcher.replacementWatchStarted) })
	case key == watcher.successorKey:
		watcher.successorOnce.Do(func() { close(watcher.successorWatchStarted) })
	}
	return watcher.Watcher.Watch(ctx, key, opts...)
}

func (watcher *closeFirstElectionWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	watcher.lock.Lock()
	if strings.Contains(key, "election/") {
		watcher.calls++
	}
	if watcher.calls == watcher.closeAt && strings.Contains(key, "election/") {
		watcher.calls++
		close(watcher.done)
		watcher.lock.Unlock()
		responses := make(chan clientv3.WatchResponse)
		close(responses)
		return responses
	}
	watcher.lock.Unlock()
	return watcher.Watcher.Watch(ctx, key, opts...)
}

func (watcher *invalidFirstWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	watcher.lock.Lock()
	watcher.calls++
	call := watcher.calls
	watcher.lock.Unlock()
	if call == 1 {
		responses := make(chan clientv3.WatchResponse, 1)
		responses <- watcher.response
		close(responses)
		return responses
	}
	return watcher.Watcher.Watch(ctx, key, opts...)
}

func (kv *firstPutBlockingKV) Put(ctx context.Context, key, value string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	call := kv.nextCall()
	if call == 1 {
		if kv.deadline != nil {
			_, hasDeadline := ctx.Deadline()
			kv.deadline <- hasDeadline
		}
		close(kv.firstStarted)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-kv.releaseFirst:
		}
	}
	return kv.KV.Put(ctx, key, value, opts...)
}

func (kv *firstPutBlockingKV) Txn(ctx context.Context) clientv3.Txn {
	return &commitHookTxn{
		Txn: kv.KV.Txn(ctx),
		commit: func(txn clientv3.Txn) (*clientv3.TxnResponse, error) {
			call := kv.nextCall()
			if call == 1 {
				if kv.deadline != nil {
					_, hasDeadline := ctx.Deadline()
					kv.deadline <- hasDeadline
				}
				close(kv.firstStarted)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-kv.releaseFirst:
				}
			}
			return txn.Commit()
		},
	}
}

func (kv *firstPutBlockingKV) calls() int {
	kv.lock.Lock()
	defer kv.lock.Unlock()
	return kv.putCalls
}

func (kv *failingPutKV) Put(context.Context, string, string, ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	return nil, kv.err
}

func (kv *failingPutKV) Do(ctx context.Context, op clientv3.Op) (clientv3.OpResponse, error) {
	if op.IsPut() {
		return clientv3.OpResponse{}, kv.err
	}
	return kv.KV.Do(ctx, op)
}

func (kv *failingPutKV) Txn(ctx context.Context) clientv3.Txn {
	return &commitHookTxn{
		Txn: kv.KV.Txn(ctx),
		commit: func(clientv3.Txn) (*clientv3.TxnResponse, error) {
			return nil, kv.err
		},
	}
}

type trackingLease struct {
	clientv3.Lease
	lock     sync.Mutex
	contexts []context.Context
}

type failKeepAliveCallLease struct {
	clientv3.Lease
	lock    sync.Mutex
	calls   int
	failAt  int
	failed  chan struct{}
	release <-chan struct{}
}

func (lease *failKeepAliveCallLease) KeepAlive(ctx context.Context, id clientv3.LeaseID) (<-chan *clientv3.LeaseKeepAliveResponse, error) {
	lease.lock.Lock()
	lease.calls++
	call := lease.calls
	lease.lock.Unlock()
	if call == lease.failAt {
		lease.failed <- struct{}{}
		if lease.release != nil {
			<-lease.release
		}
		return nil, errors.New("injected election session keepalive failure")
	}
	return lease.Lease.KeepAlive(ctx, id)
}

type failingRevokeLease struct {
	clientv3.Lease
	err error
}

func (lease *failingRevokeLease) Revoke(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	return nil, lease.err
}

type blockingRevokeLease struct {
	clientv3.Lease
	started chan clientv3.LeaseID
	release chan struct{}
	once    sync.Once
}

func (lease *blockingRevokeLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	block := false
	lease.once.Do(func() { block = true })
	if block {
		select {
		case lease.started <- id:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-lease.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return lease.Lease.Revoke(ctx, id)
}

type cleanupBlockingKV struct {
	clientv3.KV
	lock              sync.Mutex
	txnCount          int
	campaignCommitted chan struct{}
	cleanupStarted    chan struct{}
	releaseCleanup    chan struct{}
	campaignOnce      sync.Once
	cleanupOnce       sync.Once
}

func (kv *cleanupBlockingKV) Txn(ctx context.Context) clientv3.Txn {
	kv.lock.Lock()
	kv.txnCount++
	txnCount := kv.txnCount
	kv.lock.Unlock()
	if txnCount == 1 {
		return &signalingTxn{
			Txn:       kv.KV.Txn(ctx),
			committed: kv.campaignCommitted,
			once:      &kv.campaignOnce,
		}
	}
	return &blockingTxn{
		ctx:     ctx,
		started: kv.cleanupStarted,
		release: kv.releaseCleanup,
		once:    &kv.cleanupOnce,
	}
}

type signalingTxn struct {
	clientv3.Txn
	committed chan struct{}
	once      *sync.Once
}

func (txn *signalingTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.Txn = txn.Txn.If(cmps...)
	return txn
}

func (txn *signalingTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Then(ops...)
	return txn
}

func (txn *signalingTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(ops...)
	return txn
}

func (txn *signalingTxn) Commit() (*clientv3.TxnResponse, error) {
	response, err := txn.Txn.Commit()
	txn.once.Do(func() { close(txn.committed) })
	return response, err
}

type blockingTxn struct {
	ctx     context.Context
	started chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (txn *blockingTxn) If(...clientv3.Cmp) clientv3.Txn {
	return txn
}

func (txn *blockingTxn) Then(...clientv3.Op) clientv3.Txn {
	return txn
}

func (txn *blockingTxn) Else(...clientv3.Op) clientv3.Txn {
	return txn
}

func (txn *blockingTxn) Commit() (*clientv3.TxnResponse, error) {
	txn.once.Do(func() { close(txn.started) })
	select {
	case <-txn.ctx.Done():
		return nil, txn.ctx.Err()
	case <-txn.release:
		return nil, errors.New("released blocked election cleanup")
	}
}

func (lease *trackingLease) KeepAlive(ctx context.Context, id clientv3.LeaseID) (<-chan *clientv3.LeaseKeepAliveResponse, error) {
	lease.lock.Lock()
	lease.contexts = append(lease.contexts, ctx)
	lease.lock.Unlock()
	return lease.Lease.KeepAlive(ctx, id)
}

func (lease *trackingLease) lastContext() context.Context {
	lease.lock.Lock()
	defer lease.lock.Unlock()
	if len(lease.contexts) == 0 {
		return nil
	}
	return lease.contexts[len(lease.contexts)-1]
}

func (kv *failingGetKV) Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	return nil, kv.err
}

func (kv *failingGetKV) Do(ctx context.Context, op clientv3.Op) (clientv3.OpResponse, error) {
	if op.IsGet() {
		return clientv3.OpResponse{}, kv.err
	}
	return kv.KV.Do(ctx, op)
}

func getEtcdSD(t *testing.T, config config.EtcdServiceDiscoveryConfig, server *Server, cli *clientv3.Client) *etcdServiceDiscovery {
	t.Helper()
	appDieChan := make(chan bool)
	e, err := NewEtcdServiceDiscovery(config, server, appDieChan, cli)
	assert.NoError(t, err)
	etcdSD := e.(*etcdServiceDiscovery)
	t.Cleanup(func() {
		require.NoError(t, etcdSD.Shutdown())
		select {
		case <-etcdSD.callbackDrained:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for callback dispatcher cleanup")
		}
		select {
		case <-etcdSD.leaderCallbackDrained:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for leader callback dispatcher cleanup")
		}
	})
	return etcdSD
}

func newTestClientView(ctx context.Context, cli *clientv3.Client) *clientv3.Client {
	view := clientv3.NewCtxClient(ctx, clientv3.WithZapLogger(cli.GetLogger()))
	view.KV = cli.KV
	view.Lease = cli.Lease
	view.Watcher = cli.Watcher
	return view
}

func enqueueTestCallback(t *testing.T, sd *etcdServiceDiscovery, callback func()) <-chan struct{} {
	t.Helper()
	done, accepted := sd.enqueueCallback(callback)
	require.True(t, accepted)
	return done
}

func enqueueTestLeaderCallback(t *testing.T, sd *etcdServiceDiscovery, callback func()) <-chan struct{} {
	t.Helper()
	done, accepted := sd.enqueueLeaderCallback(callback)
	require.True(t, accepted)
	return done
}

func TestNewEtcdServiceDiscovery(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			assert.NotNil(t, e)
		})
	}
}

func TestNewEtcdServiceDiscoveryRejectsInvalidIdentity(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	tests := []struct {
		name   string
		server *Server
	}{
		{name: "nil server"},
		{name: "empty id", server: NewServer("", "room", false, nil)},
		{name: "empty type", server: NewServer("room-1", "", false, nil)},
		{name: "slash in id", server: NewServer("room/1", "room", false, nil)},
		{name: "slash in type", server: NewServer("room-1", "game/room", false, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sd, err := NewEtcdServiceDiscovery(cfg, tt.server, nil)
			assert.Error(t, err)
			assert.Nil(t, sd)
		})
	}
}

func TestEtcdInjectedClientRemainsCallerOwned(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Heartbeat.TTL = 3 * time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	originalKV, originalLease, originalWatcher := cli.KV, cli.Lease, cli.Watcher

	firstServer := NewServer("frontend-first", "frontend", true, nil)
	first := getEtcdSD(t, cfg, firstServer, cli)
	require.NoError(t, first.Init())
	require.NoError(t, first.Shutdown())
	assert.NoError(t, cli.Ctx().Err())
	assert.True(t, cli.KV == originalKV)
	assert.True(t, cli.Lease == originalLease)
	assert.True(t, cli.Watcher == originalWatcher)

	secondServer := NewServer("frontend-second", "frontend", true, nil)
	second := getEtcdSD(t, cfg, secondServer, cli)
	require.NoError(t, second.Init())
	assert.True(t, cli.KV == originalKV)
	assert.True(t, cli.Lease == originalLease)
	assert.True(t, cli.Watcher == originalWatcher)

	response, err := cli.Get(context.Background(), cfg.Prefix+getKey(secondServer.ID, secondServer.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	doublePrefixed, err := cli.Get(context.Background(), cfg.Prefix+cfg.Prefix+getKey(secondServer.ID, secondServer.Type))
	require.NoError(t, err)
	assert.Empty(t, doublePrefixed.Kvs)
	require.NoError(t, second.Shutdown())
	assert.NoError(t, cli.Ctx().Err())
}

func TestEtcdSDBootstrapLease(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			e.lifecycleLock.Lock()
			e.lifecycleState = etcdLifecycleInitializing
			e.lifecycleLock.Unlock()
			err := e.grantLease(e.leaseCtx)
			assert.NoError(t, err)
			assert.NotZero(t, e.leaseID.Load())
		})
	}
}

func TestEtcdSDBootstrapLeaseError(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			e.etcdDialTimeout = -time.Nanosecond
			err := e.grantLease(e.leaseCtx)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

func TestEtcdSDBootstrapServer(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			err := e.bootstrapServer(table.server)
			assert.NoError(t, err)
			v, err := e.cli.Get(context.TODO(), getKey(table.server.ID, table.server.Type))
			assert.NoError(t, err)
			assert.NotNil(t, v)
			assert.Equal(t, 1, len(v.Kvs))
			generatedSv, ok := e.serverMapByID.Load(table.server.ID)
			assert.True(t, ok)
			assert.Equal(t, table.server, generatedSv)
			val := v.Kvs[0]
			assert.Equal(t, getKey(table.server.ID, table.server.Type), string(val.Key))
			assert.Equal(t, table.server.AsJSONString(), string(val.Value))
		})
	}
}

func TestEtcdSDDeleteServer(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			err := e.bootstrapServer(table.server)
			assert.NoError(t, err)
			e.deleteServer(table.server.ID)
			e.serverMapByID.Delete(table.server.ID)
			generatedSv, ok := e.serverMapByID.Load(table.server.ID)
			assert.False(t, ok)
			assert.Nil(t, generatedSv)
			_, err = e.GetServersByType(table.server.Type)
			assert.ErrorIs(t, err, constants.ErrNoServersAvailableOfType)
		})
	}
}

func TestEtcdSDConsistentHashLifecycle(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	_, err := sd.GetConsistentHashNode("room", "session-1")
	assert.ErrorIs(t, err, constants.ErrServerNotFound)

	server := NewServer("room-1", "room", false, nil)
	sd.addServer(server)
	node, err := sd.GetConsistentHashNode(server.Type, "session-1")
	assert.NoError(t, err)
	assert.Equal(t, server.ID, node)

	sd.deleteServer(server.ID)
	_, err = sd.GetConsistentHashNode(server.Type, "session-1")
	assert.ErrorIs(t, err, constants.ErrServerNotFound)
}

func TestEtcdSDConsistentHashConcurrentFirstServers(t *testing.T) {
	for removedServer := 0; removedServer < 2; removedServer++ {
		t.Run(fmt.Sprintf("remove-%d", removedServer), func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			self := NewServer("frontend-self", "frontend", true, nil)
			sd := getEtcdSD(t, cfg, self, nil)
			servers := []*Server{
				NewServer("room-1", "room", false, nil),
				NewServer("room-2", "room", false, nil),
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, server := range servers {
				server := server
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					sd.addServer(server)
				}()
			}
			close(start)
			wg.Wait()

			sd.deleteServer(servers[removedServer].ID)
			remainingServer := servers[1-removedServer]
			node, err := sd.GetConsistentHashNode(remainingServer.Type, "session-1")
			assert.NoError(t, err)
			assert.Equal(t, remainingServer.ID, node)

			sd.deleteServer(remainingServer.ID)
			_, err = sd.GetConsistentHashNode(remainingServer.Type, "session-1")
			assert.ErrorIs(t, err, constants.ErrServerNotFound)
		})
	}
}

func TestEtcdSDConsistentHashConcurrentMutationAndLookup(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	const (
		typesPerRound = 32
		rounds        = 8
		lookupWorkers = 8
	)

	serverTypes := make([]string, 0, rounds*typesPerRound)
	for round := 0; round < rounds; round++ {
		for typeIndex := 0; typeIndex < typesPerRound; typeIndex++ {
			serverTypes = append(serverTypes, fmt.Sprintf("room-%d-%d", round, typeIndex))
		}
	}

	start := make(chan struct{})
	unexpectedErrors := make(chan error, lookupWorkers*len(serverTypes)*rounds)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for round := 0; round < rounds; round++ {
			for typeIndex := 0; typeIndex < typesPerRound; typeIndex++ {
				serverType := fmt.Sprintf("room-%d-%d", round, typeIndex)
				sd.addServer(NewServer(serverType+"-1", serverType, false, nil))
				runtime.Gosched()
			}
			// This is the cleanup path used by SyncServers after applying its snapshot.
			sd.deleteLocalInvalidServers(nil)
		}
	}()

	for worker := 0; worker < lookupWorkers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for round := 0; round < rounds; round++ {
				for _, serverType := range serverTypes {
					node, err := sd.GetConsistentHashNode(serverType, fmt.Sprintf("session-%d-%d", worker, round))
					if err != nil && !errors.Is(err, constants.ErrServerNotFound) {
						unexpectedErrors <- err
					}
					if err == nil && node != serverType+"-1" {
						unexpectedErrors <- fmt.Errorf("unexpected node %q returned for %s", node, serverType)
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(unexpectedErrors)
	for err := range unexpectedErrors {
		assert.NoError(t, err)
	}

	sd.deleteLocalInvalidServers(nil)
	for _, serverType := range serverTypes {
		_, err := sd.GetConsistentHashNode(serverType, "session-final")
		assert.ErrorIs(t, err, constants.ErrServerNotFound)
	}
}

func TestEtcdSDGetKey(t *testing.T) {
	t.Parallel()
	tables := []struct {
		serverType string
		serverID   string
		ret        string
	}{
		{"type1", "id1", "servers/type1/id1"},
		{"t", "1", "servers/t/1"},
	}

	for _, table := range tables {
		t.Run(table.ret, func(t *testing.T) {
			assert.Equal(t, table.ret, getKey(table.serverID, table.serverType))
		})
	}
}

func TestEtcdSDDeleteLocalInvalidServers(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			invalidServer := &Server{
				ID:   "invalid",
				Type: "bla",
			}
			e.addServer(invalidServer)
			e.deleteLocalInvalidServers([]string{table.server.ID})
			inv, err := e.GetServer(invalidServer.ID)
			assert.ErrorIs(t, err, constants.ErrNoServerWithID)
			assert.Nil(t, inv)
		})
	}
}

func TestEtcdSDGetServer(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			e.bootstrapServer(table.server)
			sv, err := e.GetServer(table.server.ID)
			assert.NoError(t, err)
			assert.Equal(t, table.server, sv)
		})
	}
}

func TestEtcdSDGetServers(t *testing.T) {
	for _, table := range etcdSDTablesMultipleServers {
		t.Run(table.name, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			self := NewServer("self-1", "self", false)
			e := getEtcdSD(t, config, self, cli)
			require.NoError(t, e.Init())
			for _, server := range table.servers {
				e.bootstrapServer(server)
			}
			serverList := e.GetServers()
			var checkList []*Server
			checkList = append(table.servers, self)
			assert.ElementsMatch(t, checkList, serverList)
		})
	}
}

func TestEtcdSDInit(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			// should set running
			assert.True(t, e.Running())
			// should have a leaseid
			assert.NotZero(t, e.leaseID.Load())
			// should register the server
			sv, err := e.GetServer(table.server.ID)
			assert.NoError(t, err)
			assert.Equal(t, table.server, sv)
			// Init completes the first server snapshot before returning.
			assert.NotZero(t, e.lastSyncUnixNano.Load())
		})
	}
}

func TestEtcdSDInitPreservesBestEffortInitialSync(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	rawKV := cli.KV
	syncErr := errors.New("initial sync unavailable")
	cli.KV = &failingGetKV{KV: rawKV, err: syncErr}

	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	assert.True(t, sd.Running())
	registered, err := sd.GetServer(self.ID)
	require.NoError(t, err)
	assert.Equal(t, self, registered)

	response, err := rawKV.Get(context.Background(), cfg.Prefix+getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	assert.ErrorIs(t, sd.SyncServers(true), syncErr)
}

func TestEtcdSDWatchRecoversExistingServersAfterInitialSnapshotFailure(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.SyncServers.Interval = time.Hour
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	rawKV := cli.KV
	remote := NewServer("room-existing", "room", false, map[string]string{"version": "one"})
	_, err := rawKV.Put(context.Background(), cfg.Prefix+getKey(remote.ID, remote.Type), remote.AsJSONString())
	require.NoError(t, err)
	cli.KV = &failFirstGetKV{KV: rawKV, err: errors.New("initial snapshot unavailable")}

	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	require.NoError(t, sd.Init())
	helpers.ShouldEventuallyReturn(t, func() string {
		server, getErr := sd.GetServer(remote.ID)
		if getErr != nil {
			return ""
		}
		return server.Metadata["version"]
	}, "one", 20*time.Millisecond, 5*time.Second)
}

func TestEtcdSDInitWaitsForInitialTopologyCallbacks(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	remote := NewServer("room-existing", "room", false, nil)
	_, err := cli.Put(context.Background(), cfg.Prefix+getKey(remote.ID, remote.Type), remote.AsJSONString())
	require.NoError(t, err)

	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	sd.AddListener(&testSDListener{add: func(server *Server) {
		if server.ID == remote.ID {
			close(callbackStarted)
			<-releaseCallback
		}
	}})
	initResult := make(chan error, 1)
	go func() { initResult <- sd.Init() }()
	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		close(releaseCallback)
		t.Fatal("initial topology callback did not start")
	}
	select {
	case err := <-initResult:
		close(releaseCallback)
		t.Fatalf("Init returned before initial topology callback completed: %v", err)
	default:
	}
	close(releaseCallback)
	select {
	case err := <-initResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Init did not return after initial topology callback completed")
	}
}

func TestEtcdSDInitialTopologyListenerCanShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	remote := NewServer("room-existing", "room", false, nil)
	_, err := cli.Put(context.Background(), cfg.Prefix+getKey(remote.ID, remote.Type), remote.AsJSONString())
	require.NoError(t, err)

	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	shutdownResult := make(chan error, 1)
	sd.AddListener(&testSDListener{add: func(server *Server) {
		if server.ID == remote.ID {
			shutdownResult <- sd.Shutdown()
		}
	}})
	initResult := make(chan error, 1)
	go func() { initResult <- sd.Init() }()
	select {
	case err := <-shutdownResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown deadlocked in an initial topology callback")
	}
	select {
	case err := <-initResult:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Init did not unwind after callback shutdown")
	}
}

func TestEtcdSDConcurrentInitSharesOneResult(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	var releaseFactoryOnce sync.Once
	release := func() { releaseFactoryOnce.Do(func() { close(releaseFactory) }) }
	t.Cleanup(release)
	factoryErr := errors.New("etcd factory unavailable")
	var factoryLock sync.Mutex
	factoryCalls := 0
	sd.newEtcdClient = func(clientv3.Config) (*clientv3.Client, error) {
		factoryLock.Lock()
		factoryCalls++
		factoryLock.Unlock()
		close(factoryStarted)
		<-releaseFactory
		return nil, factoryErr
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			results <- sd.Init()
		}()
	}
	close(start)
	<-factoryStarted
	release()
	for i := 0; i < 2; i++ {
		assert.ErrorIs(t, <-results, factoryErr)
	}
	factoryLock.Lock()
	assert.Equal(t, 1, factoryCalls)
	factoryLock.Unlock()
}

func TestEtcdSDShutdownRejectsLateOwnedClientPublication(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, source := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	candidate, err := clientv3.New(clientv3.Config{
		Endpoints:   source.Endpoints(),
		DialTimeout: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = candidate.Close() })

	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	var releaseFactoryOnce sync.Once
	release := func() { releaseFactoryOnce.Do(func() { close(releaseFactory) }) }
	t.Cleanup(release)
	sd.newEtcdClient = func(clientv3.Config) (*clientv3.Client, error) {
		close(factoryStarted)
		<-releaseFactory
		return candidate, nil
	}

	initResult := make(chan error, 1)
	go func() { initResult <- sd.Init() }()
	<-factoryStarted
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- sd.Shutdown() }()
	helpers.ShouldEventuallyReturn(t, func() etcdLifecycleState {
		sd.lifecycleLock.Lock()
		defer sd.lifecycleLock.Unlock()
		return sd.lifecycleState
	}, etcdLifecycleStopping)
	release()

	assert.ErrorIs(t, <-initResult, context.Canceled)
	require.NoError(t, <-shutdownResult)
	select {
	case <-candidate.Ctx().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("owned client candidate remained open after losing publication race")
	}
	sd.lifecycleLock.Lock()
	assert.Nil(t, sd.cli)
	assert.False(t, sd.ownsClient)
	assert.Equal(t, etcdLifecycleStopped, sd.lifecycleState)
	sd.lifecycleLock.Unlock()
}

func TestEtcdSDShutdownBeforeInitPreventsClientCreation(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	factoryCalls := 0
	sd.newEtcdClient = func(clientv3.Config) (*clientv3.Client, error) {
		factoryCalls++
		return nil, errors.New("factory should not run")
	}
	require.NoError(t, sd.Shutdown())
	assert.ErrorIs(t, sd.Init(), context.Canceled)
	assert.Zero(t, factoryCalls)
}

func TestEtcdSDSyncServersWaitsForConcurrentCallback(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	drained := enqueueTestCallback(t, sd, nil)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining initial callbacks")
	}
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	release := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	callbackDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	result := make(chan error, 1)
	go func() { result <- sd.SyncServers(true) }()
	helpers.ShouldEventuallyReturn(t, func() int {
		sd.callbackLock.Lock()
		defer sd.callbackLock.Unlock()
		return len(sd.callbackQueue)
	}, 1)

	select {
	case err := <-result:
		t.Fatalf("SyncServers returned before its callback barrier: %v", err)
	default:
	}
	release()
	<-callbackDone
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SyncServers callback barrier")
	}
}

func TestEtcdSDLeaderCallbackIsIndependentAndObservesMatchingState(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	release := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	type leaderObservation struct {
		event    string
		observed string
	}
	observations := make(chan leaderObservation, 2)
	sd.OnLeaderChange(func(leader string) {
		observations <- leaderObservation{event: leader, observed: sd.LeaderID()}
	})
	require.True(t, sd.activateLeaderGeneration(1))

	for _, expected := range []string{"frontend-other", self.ID} {
		require.True(t, sd.notifyLeaderChange(1, expected, false))
		select {
		case observation := <-observations:
			assert.Equal(t, expected, observation.event)
			assert.Equal(t, expected, observation.observed)
		case <-time.After(5 * time.Second):
			t.Fatalf("leader observation %q waited for a blocked topology callback", expected)
		}
	}
	release()
	<-blockerDone
}

func TestEtcdSDLeaderGenerationRejectsStaleEventsAndKeepsFIFO(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestLeaderCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	type observation struct {
		event    string
		state    string
		isLeader bool
	}
	observations := make(chan observation, 4)
	sd.OnLeaderChange(func(leader string) {
		observations <- observation{event: leader, state: sd.LeaderID(), isLeader: sd.IsLeader()}
	})

	require.True(t, sd.activateLeaderGeneration(1))
	require.True(t, sd.notifyLeaderChange(1, "frontend-old", false))
	require.True(t, sd.activateLeaderGeneration(2))
	assert.False(t, sd.notifyLeaderChange(1, "frontend-stale", false))
	require.True(t, sd.notifyLeaderChange(2, "", false))
	require.True(t, sd.notifyLeaderChange(2, self.ID, false))
	drained := enqueueTestLeaderCallback(t, sd, nil)

	release()
	<-blockerDone
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining leader callbacks")
	}

	for _, expected := range []string{"frontend-old", "", self.ID} {
		select {
		case actual := <-observations:
			assert.Equal(t, expected, actual.event)
			assert.Equal(t, expected, actual.state)
			assert.Equal(t, expected == self.ID, actual.isLeader)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for leader event %q", expected)
		}
	}
	select {
	case unexpected := <-observations:
		t.Fatalf("received stale leader event: %+v", unexpected)
	default:
	}
}

func TestEtcdSDLeaderQueueBackpressureAllowsListenerStateRead(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	listenerStarted := make(chan struct{})
	readState := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(readState) }) }
	t.Cleanup(release)
	type observation struct {
		event    string
		isLeader bool
	}
	observations := make(chan observation, 2)
	sd.OnLeaderChange(func(leader string) {
		if leader == self.ID {
			close(listenerStarted)
			<-readState
		}
		observations <- observation{event: leader, isLeader: sd.IsLeader()}
	})

	require.True(t, sd.activateLeaderGeneration(1))
	require.True(t, sd.notifyLeaderChange(1, self.ID, false))
	<-listenerStarted
	for i := 0; i < callbackQueueLimit; i++ {
		enqueueTestLeaderCallback(t, sd, nil)
	}

	published := make(chan bool, 1)
	go func() {
		published <- sd.notifyLeaderChange(1, "frontend-other", false)
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		sd.leaderCallbackLock.Lock()
		defer sd.leaderCallbackLock.Unlock()
		return sd.leaderCallbackWaiters
	}, 1)

	release()
	select {
	case actual := <-observations:
		assert.Equal(t, self.ID, actual.event)
		assert.True(t, actual.isLeader)
	case <-time.After(5 * time.Second):
		t.Fatal("leader listener could not read state under queue backpressure")
	}
	select {
	case accepted := <-published:
		assert.True(t, accepted)
	case <-time.After(5 * time.Second):
		t.Fatal("leader publication did not enter the released queue slot")
	}
	drained := enqueueTestLeaderCallback(t, sd, nil)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining leader callback queue")
	}
	select {
	case actual := <-observations:
		assert.Equal(t, "frontend-other", actual.event)
		assert.False(t, actual.isLeader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backpressured leader callback")
	}
}

func TestEtcdSDLeaderQueueStopWakesSaturatedPublisher(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestLeaderCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted
	for i := 0; i < callbackQueueLimit; i++ {
		enqueueTestLeaderCallback(t, sd, nil)
	}

	require.True(t, sd.activateLeaderGeneration(1))
	published := make(chan bool, 1)
	go func() {
		published <- sd.notifyLeaderChange(1, "frontend-other", false)
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		sd.leaderCallbackLock.Lock()
		defer sd.leaderCallbackLock.Unlock()
		return sd.leaderCallbackWaiters
	}, 1)
	select {
	case accepted := <-published:
		t.Fatalf("saturated leader publication returned early: %v", accepted)
	default:
	}

	sd.stopLeaderCallbacks()
	select {
	case accepted := <-published:
		assert.False(t, accepted)
	case <-time.After(5 * time.Second):
		t.Fatal("stopping leader callbacks did not wake saturated publication")
	}
	release()
	<-blockerDone
	select {
	case <-sd.leaderCallbackDrained:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining saturated leader callback queue")
	}
}

func TestEtcdSDCallbackQueueAppliesBackpressure(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	release := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted
	for i := 0; i < callbackQueueLimit; i++ {
		enqueueTestCallback(t, sd, nil)
	}
	type enqueueResult struct {
		done     <-chan struct{}
		accepted bool
	}
	extraEnqueued := make(chan enqueueResult, 1)
	go func() {
		done, accepted := sd.enqueueCallback(nil)
		extraEnqueued <- enqueueResult{done: done, accepted: accepted}
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		sd.callbackLock.Lock()
		defer sd.callbackLock.Unlock()
		return sd.callbackWaiters
	}, 1)

	select {
	case <-extraEnqueued:
		t.Fatal("callback queue accepted work beyond its bounded capacity")
	default:
	}
	release()
	<-blockerDone
	select {
	case result := <-extraEnqueued:
		assert.True(t, result.accepted)
		assert.NotNil(t, result.done)
	case <-time.After(5 * time.Second):
		t.Fatal("callback enqueue remained blocked after queue progress")
	}
}

func TestEtcdSDCallbackQueueSeparatesProducerAndConsumerWakeups(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	release := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	for i := 0; i < callbackQueueLimit; i++ {
		enqueueTestCallback(t, sd, nil)
	}

	type enqueueResult struct {
		done     <-chan struct{}
		accepted bool
	}
	const producerCount = callbackQueueLimit * 2
	results := make(chan enqueueResult, producerCount)
	for i := 0; i < producerCount; i++ {
		go func() {
			done, accepted := sd.enqueueCallback(nil)
			results <- enqueueResult{done: done, accepted: accepted}
		}()
	}
	helpers.ShouldEventuallyReturn(t, func() int {
		sd.callbackLock.Lock()
		defer sd.callbackLock.Unlock()
		return sd.callbackWaiters
	}, producerCount)

	release()
	<-blockerDone
	acceptedWork := make([]<-chan struct{}, 0, producerCount)
	deadline := time.After(5 * time.Second)
	for i := 0; i < producerCount; i++ {
		select {
		case result := <-results:
			require.True(t, result.accepted)
			require.NotNil(t, result.done)
			acceptedWork = append(acceptedWork, result.done)
		case <-deadline:
			t.Fatal("callback producers and dispatcher stopped waking each other")
		}
	}
	for _, done := range acceptedWork {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("timed out draining callbacks accepted from blocked producers")
		}
	}
}

func TestEtcdSDShutdownDrainsAcceptedCallbacks(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	release := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	events := make(chan int, 2)
	firstDone := enqueueTestCallback(t, sd, func() { events <- 1 })
	secondDone := enqueueTestCallback(t, sd, func() { events <- 2 })
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- sd.Shutdown() }()
	select {
	case err := <-shutdownResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown waited for the callback that invoked its drain boundary")
	}
	release()
	<-blockerDone
	<-firstDone
	<-secondDone
	for _, expected := range []int{1, 2} {
		select {
		case actual := <-events:
			assert.Equal(t, expected, actual)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for drained callback %d", expected)
		}
	}
}

func TestEtcdSDSyncServersWaitsForDrainWhenBarrierIsRejected(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	release := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	callbackDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted
	sd.stopCallbacks()

	result := make(chan error, 1)
	go func() { result <- sd.SyncServers(true) }()
	helpers.ShouldEventuallyReturn(t, sd.callbackBarrierWaiters.Load, int32(1), 10*time.Millisecond, 5*time.Second)
	select {
	case err := <-result:
		t.Fatalf("SyncServers returned before accepted callbacks drained: %v", err)
	default:
	}
	release()
	<-callbackDone
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SyncServers drain boundary")
	}
}

func TestEtcdSDWatchEventsApplyMonotonicServerRevisions(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	serverAt := func(version string) *Server {
		return NewServer("room-1", "room", false, map[string]string{"version": version})
	}
	putPayload := func(payload string, revision int64) clientv3.WatchResponse {
		return clientv3.WatchResponse{
			Header: etcdserverpb.ResponseHeader{Revision: revision},
			Events: []*clientv3.Event{{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte(getKey("room-1", "room")),
					Value:       []byte(payload),
					ModRevision: revision,
				},
			}},
		}
	}
	put := func(server *Server, revision int64) clientv3.WatchResponse {
		return putPayload(server.AsJSONString(), revision)
	}
	deleteAt := func(revision int64) clientv3.WatchResponse {
		return clientv3.WatchResponse{
			Header: etcdserverpb.ResponseHeader{Revision: revision},
			Events: []*clientv3.Event{{
				Type: mvccpb.DELETE,
				Kv: &mvccpb.KeyValue{
					Key:         []byte(getKey("room-1", "room")),
					ModRevision: revision,
				},
			}},
		}
	}

	sd.applyEtcdWatchResponse(put(serverAt("new"), 20))
	sd.applyEtcdWatchResponse(put(serverAt("old"), 10))
	sd.applyEtcdWatchResponse(deleteAt(15))
	server, err := sd.GetServer("room-1")
	require.NoError(t, err)
	assert.Equal(t, "new", server.Metadata["version"])

	sd.applyEtcdWatchResponse(deleteAt(21))
	_, err = sd.GetServer("room-1")
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)
	sd.applyEtcdWatchResponse(put(serverAt("stale-resurrection"), 20))
	_, err = sd.GetServer("room-1")
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)

	sd.applyEtcdWatchResponse(put(serverAt("recovered"), 22))
	server, err = sd.GetServer("room-1")
	require.NoError(t, err)
	assert.Equal(t, "recovered", server.Metadata["version"])

	malformedPayloads := []string{
		"",
		"{",
		"null",
		"{}",
		NewServer("room-other", "room", false, nil).AsJSONString(),
		NewServer("room-1", "other", false, nil).AsJSONString(),
	}
	for i, payload := range malformedPayloads {
		revision := int64(23 + i)
		sd.applyEtcdWatchResponse(putPayload(payload, revision))
		server, err = sd.GetServer("room-1")
		require.NoError(t, err)
		assert.Equal(t, "recovered", server.Metadata["version"])
		assert.Equal(t, revision, sd.serverRevisions["room-1"])
	}

	recoveryRevision := int64(23 + len(malformedPayloads))
	sd.applyEtcdWatchResponse(put(serverAt("recovered-again"), recoveryRevision))
	server, err = sd.GetServer("room-1")
	require.NoError(t, err)
	assert.Equal(t, "recovered-again", server.Metadata["version"])
}

func TestEtcdSDSnapshotCannotOverwriteNewerWatchState(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	newServer := NewServer("room-1", "room", false, map[string]string{"version": "new"})
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 20},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(newServer.ID, newServer.Type)),
				Value:       []byte(newServer.AsJSONString()),
				ModRevision: 20,
			},
		}},
	})

	oldServer := NewServer(newServer.ID, newServer.Type, false, map[string]string{"version": "old"})
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  10,
		serverIDs: map[string]struct{}{newServer.ID: {}},
		servers:   []versionedServer{{server: oldServer, revision: 10}},
	}, false)
	actual, err := sd.GetServer(newServer.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", actual.Metadata["version"])

	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  10,
		serverIDs: map[string]struct{}{},
	}, false)
	actual, err = sd.GetServer(newServer.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", actual.Metadata["version"])
}

func TestEtcdSDSnapshotPrunesRevisionTombstones(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	const tombstoneCount = 1024
	events := make([]*clientv3.Event, 0, tombstoneCount+1)
	for i := 0; i < tombstoneCount; i++ {
		serverID := fmt.Sprintf("gone-%04d", i)
		events = append(events, &clientv3.Event{
			Type: mvccpb.DELETE,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(serverID, "room")),
				ModRevision: int64(i + 1),
			},
		})
	}
	live := NewServer("room-live", "room", false, nil)
	liveRevision := int64(tombstoneCount + 1)
	events = append(events, &clientv3.Event{
		Type: mvccpb.PUT,
		Kv: &mvccpb.KeyValue{
			Key:         []byte(getKey(live.ID, live.Type)),
			Value:       []byte(live.AsJSONString()),
			ModRevision: liveRevision,
		},
	})
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: liveRevision},
		Events: events,
	})
	require.Len(t, sd.serverRevisions, tombstoneCount+1)

	snapshotRevision := int64(tombstoneCount + 100)
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  snapshotRevision,
		serverIDs: map[string]struct{}{live.ID: {}},
	}, false)

	assert.Equal(t, snapshotRevision, sd.serverRevisionFloor)
	require.Len(t, sd.serverRevisions, 1)
	assert.Equal(t, liveRevision, sd.serverRevisions[live.ID])

	stale := NewServer("gone-0000", "room", false, map[string]string{"version": "stale"})
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: snapshotRevision},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(stale.ID, stale.Type)),
				Value:       []byte(stale.AsJSONString()),
				ModRevision: snapshotRevision,
			},
		}},
	})
	_, err := sd.GetServer(stale.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)
	assert.NotContains(t, sd.serverRevisions, stale.ID)

	fresh := NewServer(stale.ID, stale.Type, false, map[string]string{"version": "fresh"})
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: snapshotRevision + 1},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(fresh.ID, fresh.Type)),
				Value:       []byte(fresh.AsJSONString()),
				ModRevision: snapshotRevision + 1,
			},
		}},
	})
	actual, err := sd.GetServer(fresh.ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh", actual.Metadata["version"])
}

func TestEtcdSDSnapshotPruneRetainsNewerWatchTombstone(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	server := NewServer("room-1", "room", false, map[string]string{"version": "old"})

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 10},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				Value:       []byte(server.AsJSONString()),
				ModRevision: 10,
			},
		}},
	})
	staleSnapshot := &etcdServerSnapshot{
		revision:  20,
		serverIDs: map[string]struct{}{server.ID: {}},
		servers:   []versionedServer{{server: server, serverID: server.ID, revision: 10}},
	}
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 30},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				ModRevision: 30,
			},
		}},
	})

	_, _ = sd.applyServerSnapshot(staleSnapshot, false)
	_, err := sd.GetServer(server.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)
	assert.Equal(t, int64(30), sd.serverRevisions[server.ID])

	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  40,
		serverIDs: map[string]struct{}{},
	}, false)
	assert.NotContains(t, sd.serverRevisions, server.ID)

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 40},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				Value:       []byte(server.AsJSONString()),
				ModRevision: 30,
			},
		}},
	})
	_, err = sd.GetServer(server.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)

	server.Metadata["version"] = "fresh"
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 41},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				Value:       []byte(server.AsJSONString()),
				ModRevision: 41,
			},
		}},
	})
	actual, err := sd.GetServer(server.ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh", actual.Metadata["version"])
}

func TestEtcdSDWatchCheckpointUsesCompleteEventRevision(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  10,
		serverIDs: map[string]struct{}{},
	}, false)

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header:  etcdserverpb.ResponseHeader{Revision: 100},
		Created: true,
	})
	assert.Equal(t, int64(10), sd.watchRevision)
	assert.Equal(t, int64(10), sd.serverRevisionFloor)

	server := NewServer("room-1", "room", false, nil)
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 100},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				Value:       []byte(server.AsJSONString()),
				ModRevision: 20,
			},
		}},
	})
	assert.Equal(t, int64(20), sd.watchRevision)
	assert.Equal(t, int64(20), sd.serverRevisionFloor)

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 25},
	})
	assert.Equal(t, int64(25), sd.watchRevision)
	assert.Equal(t, int64(25), sd.serverRevisionFloor)
}

func TestEtcdSDWatchCheckpointAdvancesBeforeFirstSnapshotWithoutGC(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	server := NewServer("room-1", "room", false, nil)

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 100},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				ModRevision: 20,
			},
		}},
	})
	assert.Equal(t, int64(20), sd.watchRevision)
	assert.Zero(t, sd.serverRevisionFloor)
	assert.Equal(t, int64(20), sd.serverRevisions[server.ID])

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 25},
	})
	assert.Equal(t, int64(25), sd.watchRevision)
	assert.Zero(t, sd.serverRevisionFloor)
	assert.Equal(t, int64(20), sd.serverRevisions[server.ID])
}

func TestEtcdSDWatchPrunesRevisionTombstonesAfterBase(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  10,
		serverIDs: map[string]struct{}{},
	}, false)

	const eventCount = 1024
	events := make([]*clientv3.Event, 0, eventCount)
	for i := 0; i < eventCount; i++ {
		serverID := fmt.Sprintf("gone-%04d", i)
		eventType := mvccpb.DELETE
		value := []byte(nil)
		if i%2 == 1 {
			eventType = mvccpb.PUT
			value = []byte("{")
		}
		events = append(events, &clientv3.Event{
			Type: eventType,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(serverID, "room")),
				Value:       value,
				ModRevision: int64(11 + i),
			},
		})
	}
	lastEventRevision := int64(10 + eventCount)
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: lastEventRevision + 100},
		Events: events,
	})

	assert.Equal(t, lastEventRevision, sd.watchRevision)
	assert.Equal(t, lastEventRevision, sd.serverRevisionFloor)
	assert.Empty(t, sd.serverRevisions)
}

func TestEtcdSDWatchGCRejectsStaleSnapshotAndEvents(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  10,
		serverIDs: map[string]struct{}{},
	}, false)

	server := NewServer("room-1", "room", false, map[string]string{"version": "stale"})
	staleSnapshot := &etcdServerSnapshot{
		revision:  20,
		serverIDs: map[string]struct{}{server.ID: {}},
		servers: []versionedServer{{
			serverID: server.ID,
			server:   server,
			revision: 20,
		}},
	}
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 30},
		Events: []*clientv3.Event{{
			Type: mvccpb.DELETE,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				ModRevision: 30,
			},
		}},
	})
	assert.NotContains(t, sd.serverRevisions, server.ID)

	_, _ = sd.applyServerSnapshot(staleSnapshot, false)
	_, err := sd.GetServer(server.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 30},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				Value:       []byte(server.AsJSONString()),
				ModRevision: 25,
			},
		}},
	})
	_, err = sd.GetServer(server.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)

	server.Metadata["version"] = "fresh"
	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 31},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte(getKey(server.ID, server.Type)),
				Value:       []byte(server.AsJSONString()),
				ModRevision: 31,
			},
		}},
	})
	actual, err := sd.GetServer(server.ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh", actual.Metadata["version"])

	legacy := NewServer("room-legacy", "room", false, nil)
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  40,
		serverIDs: map[string]struct{}{server.ID: {}, legacy.ID: {}},
		servers: []versionedServer{{
			serverID: legacy.ID,
			server:   legacy,
			revision: 5,
		}},
	}, false)
	_, err = sd.GetServer(legacy.ID)
	require.NoError(t, err)
}

func TestEtcdSDInvalidWatchResponseDoesNotPartiallyAdvance(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)
	_, _ = sd.applyServerSnapshot(&etcdServerSnapshot{
		revision:  10,
		serverIDs: map[string]struct{}{},
	}, false)
	server := NewServer("room-1", "room", false, nil)

	sd.applyEtcdWatchResponse(clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 20},
		Events: []*clientv3.Event{
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte(getKey(server.ID, server.Type)),
					Value:       []byte(server.AsJSONString()),
					ModRevision: 20,
				},
			},
			nil,
		},
	})
	_, err := sd.GetServer(server.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)
	assert.Equal(t, int64(10), sd.watchRevision)
	assert.Equal(t, int64(10), sd.serverRevisionFloor)
}

func TestEtcdSDInvalidWatchResponseRecoversFromSnapshot(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.namespaceClient(sd.cli)

	actual := NewServer("room-actual", "room", false, nil)
	_, err := sd.cli.Put(context.Background(), getKey(actual.ID, actual.Type), actual.AsJSONString())
	require.NoError(t, err)
	bogus := NewServer("room-bogus", "room", false, nil)
	sd.cli.Watcher = &invalidFirstWatcher{
		Watcher: sd.cli.Watcher,
		response: clientv3.WatchResponse{
			Header: etcdserverpb.ResponseHeader{Revision: 100},
			Events: []*clientv3.Event{
				{
					Type: mvccpb.PUT,
					Kv: &mvccpb.KeyValue{
						Key:         []byte(getKey(bogus.ID, bogus.Type)),
						Value:       []byte(bogus.AsJSONString()),
						ModRevision: 100,
					},
				},
				nil,
			},
		},
	}

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		sd.watchEtcdChanges(watchCtx)
	}()
	t.Cleanup(func() {
		cancelWatch()
		<-watchDone
	})
	helpers.ShouldEventuallyReturn(t, func() bool {
		_, getErr := sd.GetServer(actual.ID)
		return getErr == nil
	}, true, 20*time.Millisecond, 5*time.Second)
	_, err = sd.GetServer(bogus.ID)
	assert.ErrorIs(t, err, constants.ErrNoServerWithID)
	sd.syncServersLock.Lock()
	revisionFloor := sd.serverRevisionFloor
	sd.syncServersLock.Unlock()
	assert.Positive(t, revisionFloor)
}

func TestEtcdSDKeysOnlySyncRepairsExistingServerRevision(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	server := NewServer("room-1", "room", false, map[string]string{"version": "one"})
	_, err := sd.cli.Put(context.Background(), getKey(server.ID, server.Type), server.AsJSONString())
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(true))

	server.Metadata["version"] = "two"
	_, err = sd.cli.Put(context.Background(), getKey(server.ID, server.Type), server.AsJSONString())
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(false))

	actual, err := sd.GetServer(server.ID)
	require.NoError(t, err)
	assert.Equal(t, "two", actual.Metadata["version"])

	malformedPayloads := []struct {
		name    string
		payload string
	}{
		{name: "empty value", payload: ""},
		{name: "invalid json", payload: "{"},
		{name: "null", payload: "null"},
		{name: "empty object", payload: "{}"},
		{name: "id mismatch", payload: NewServer("room-other", server.Type, false, nil).AsJSONString()},
		{name: "type mismatch", payload: NewServer(server.ID, "other", false, nil).AsJSONString()},
	}
	for _, malformed := range malformedPayloads {
		t.Run(malformed.name, func(t *testing.T) {
			put, putErr := sd.cli.Put(context.Background(), getKey(server.ID, server.Type), malformed.payload)
			require.NoError(t, putErr)
			require.NoError(t, sd.SyncServers(false))

			actual, getErr := sd.GetServer(server.ID)
			require.NoError(t, getErr)
			assert.Equal(t, "two", actual.Metadata["version"])
			sd.syncServersLock.Lock()
			assert.Equal(t, put.Header.Revision, sd.serverRevisions[server.ID])
			sd.syncServersLock.Unlock()
		})
	}

	server.Metadata["version"] = "three"
	_, err = sd.cli.Put(context.Background(), getKey(server.ID, server.Type), server.AsJSONString())
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(false))
	actual, err = sd.GetServer(server.ID)
	require.NoError(t, err)
	assert.Equal(t, "three", actual.Metadata["version"])
}

func TestEtcdSDWatchRecoversFromCompactedCheckpoint(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	server := NewServer("room-1", "room", false, map[string]string{"version": "one"})
	firstPut, err := sd.cli.Put(context.Background(), getKey(server.ID, server.Type), server.AsJSONString())
	require.NoError(t, err)
	server.Metadata["version"] = "two"
	secondPut, err := sd.cli.Put(context.Background(), getKey(server.ID, server.Type), server.AsJSONString())
	require.NoError(t, err)
	_, err = sd.cli.Compact(context.Background(), secondPut.Header.Revision)
	require.NoError(t, err)

	sd.addServer(NewServer(server.ID, server.Type, false, map[string]string{"version": "stale"}))
	sd.syncServersLock.Lock()
	sd.serverRevisions[server.ID] = firstPut.Header.Revision
	sd.watchRevision = firstPut.Header.Revision - 1
	sd.syncServersLock.Unlock()

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		sd.watchEtcdChanges(watchCtx)
	}()
	t.Cleanup(func() {
		cancelWatch()
		<-watchDone
	})
	helpers.ShouldEventuallyReturn(t, func() string {
		actual, getErr := sd.GetServer(server.ID)
		if getErr != nil {
			return ""
		}
		return actual.Metadata["version"]
	}, "two", 20*time.Millisecond, 5*time.Second)

	server.Metadata["version"] = "three"
	_, err = sd.cli.Put(context.Background(), getKey(server.ID, server.Type), server.AsJSONString())
	require.NoError(t, err)
	helpers.ShouldEventuallyReturn(t, func() string {
		actual, getErr := sd.GetServer(server.ID)
		if getErr != nil {
			return ""
		}
		return actual.Metadata["version"]
	}, "three", 20*time.Millisecond, 5*time.Second)
}

func TestEtcdSDListenerCanShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	result := make(chan error, 1)
	sd.AddListener(&testSDListener{add: func(*Server) {
		result <- sd.Shutdown()
	}})
	remote := NewServer("backend-1", "backend", false, nil)
	_, err := sd.cli.Put(context.Background(), getKey(remote.ID, remote.Type), remote.AsJSONString())
	require.NoError(t, err)

	select {
	case err := <-result:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for service discovery shutdown callback")
	}
}

func TestEtcdSDLeaderListenerCanShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	sd.lifecycleLock.Lock()
	sd.lifecycleState = etcdLifecycleRunning
	sd.lifecycleLock.Unlock()
	sd.running.Store(true)

	result := make(chan error, 1)
	sd.OnLeaderChange(func(string) {
		result <- sd.Shutdown()
	})
	require.True(t, sd.activateLeaderGeneration(1))
	require.True(t, sd.notifyLeaderChange(1, sd.server.ID, false))

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for service discovery shutdown from leader callback")
	}
	select {
	case <-sd.leaderCallbackDrained:
	case <-time.After(5 * time.Second):
		t.Fatal("leader callback dispatcher did not drain after shutdown")
	}
}

func TestEtcdSDLeaderListenerCanSyncServers(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	require.NoError(t, sd.Init())

	result := make(chan error, 1)
	sd.OnLeaderChange(func(string) {
		result <- sd.SyncServers(true)
	})
	require.True(t, sd.activateLeaderGeneration(1))
	require.True(t, sd.notifyLeaderChange(1, sd.server.ID, false))
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("leader callback deadlocked while synchronizing servers")
	}
}

func TestEtcdSDListenerCallbacksRemainOrdered(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	events := make(chan string, 2)
	sd.AddListener(&testSDListener{
		add:    func(*Server) { events <- "add" },
		remove: func(*Server) { events <- "remove" },
	})
	remote := NewServer("backend-1", "backend", false, nil)
	_, err := sd.cli.Put(context.Background(), getKey(remote.ID, remote.Type), remote.AsJSONString())
	require.NoError(t, err)
	helpers.ShouldEventuallyReturn(t, func() bool {
		_, err := sd.GetServer(remote.ID)
		return err == nil
	}, true)
	_, err = sd.cli.Delete(context.Background(), getKey(remote.ID, remote.Type))
	require.NoError(t, err)

	for _, expected := range []string{"add", "remove"} {
		select {
		case event := <-events:
			assert.Equal(t, expected, event)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s callback", expected)
		}
	}
}

func TestEtcdSDRenewFailureCancelsCandidateLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	activeLeaseID := sd.leaseID.Load()

	putErr := errors.New("candidate registration unavailable")
	sd.cli.KV = &failingPutKV{KV: sd.cli.KV, err: putErr}
	lease := &trackingLease{Lease: sd.cli.Lease}
	sd.cli.Lease = lease
	assert.ErrorIs(t, sd.renewLease(sd.lifecycleCtx, clientv3.LeaseID(activeLeaseID)), putErr)
	assert.Equal(t, activeLeaseID, sd.leaseID.Load())

	candidateCtx := lease.lastContext()
	require.NotNil(t, candidateCtx)
	select {
	case <-candidateCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("candidate keepalive context remained active after renew failure")
	}
}

func TestEtcdSDRenewalStopsFlushWhenCandidateCannotBeFenced(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	transitionErr := errors.New("ambiguous candidate transaction")
	revokeErr := errors.New("ambiguous candidate revoke")
	sd.cli.KV = &failingPutKV{KV: sd.cli.KV, err: transitionErr}
	sd.cli.Lease = &failingRevokeLease{Lease: sd.cli.Lease, err: revokeErr}
	err := sd.renewLease(sd.lifecycleCtx, clientv3.LeaseID(sd.leaseID.Load()))
	assert.ErrorIs(t, err, errEtcdCandidateLeaseUnfenced)
	assert.ErrorIs(t, err, revokeErr)
	assert.ErrorIs(t, sd.FlushServer2Cluster(self), context.Canceled)
}

func TestEtcdSDRenewalPreservesLastSuccessfulSelfPayload(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("room-self", "room", false, map[string]string{"status": "starting"})
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	cachedSelf, err := sd.GetServer(self.ID)
	require.NoError(t, err)
	updatedSelf := *cachedSelf
	updatedSelf.Metadata = map[string]string{"status": "running", "capacity": "17"}
	require.NoError(t, sd.FlushServer2Cluster(&updatedSelf))

	oldLeaseID := clientv3.LeaseID(sd.leaseID.Load())
	require.NoError(t, sd.renewLease(sd.lifecycleCtx, oldLeaseID))
	newLeaseID := sd.leaseID.Load()
	require.NotEqual(t, int64(oldLeaseID), newLeaseID)

	response, err := sd.cli.Get(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	registered, err := parseServer(response.Kvs[0].Value)
	require.NoError(t, err)
	assert.Equal(t, updatedSelf.Metadata, registered.Metadata)
	assert.Equal(t, newLeaseID, response.Kvs[0].Lease)
}

func TestEtcdSDRenewalReconcilesCommittedPutAtDeadline(t *testing.T) {
	for _, returnError := range []bool{false, true} {
		name := "success-after-deadline"
		if returnError {
			name = "deadline-error-after-commit"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			embedded, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { embedded.Terminate(t) })
			self := NewServer("frontend-self", "frontend", true, nil)
			sd := getEtcdSD(t, cfg, self, cli)
			require.NoError(t, sd.Init())
			sd.grantLeaseTimeout = time.Second
			oldLeaseID := clientv3.LeaseID(sd.leaseID.Load())
			sd.cli.KV = &postCommitTimeoutPutKV{KV: sd.cli.KV, returnError: returnError}

			require.NoError(t, sd.renewLease(sd.lifecycleCtx, oldLeaseID))
			newLeaseID := sd.leaseID.Load()
			require.NotEqual(t, int64(oldLeaseID), newLeaseID)
			response, err := sd.cli.Get(context.Background(), getKey(self.ID, self.Type))
			require.NoError(t, err)
			require.Len(t, response.Kvs, 1)
			assert.Equal(t, newLeaseID, response.Kvs[0].Lease)
		})
	}
}

func TestEtcdSDRenewalFencesCandidateTransactionThatCommitsAfterReconciliation(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, map[string]string{"version": "published"})
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	oldLeaseID := clientv3.LeaseID(sd.leaseID.Load())

	ambiguousErr := errors.New("ambiguous candidate transaction")
	deferred := &deferredMutationKV{
		KV:      sd.cli.KV,
		err:     ambiguousErr,
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  make(chan deferredMutationResult, 1),
	}
	sd.cli.KV = deferred
	assert.ErrorIs(t, sd.renewLease(sd.lifecycleCtx, oldLeaseID), ambiguousErr)
	select {
	case <-deferred.started:
	default:
		t.Fatal("candidate transaction was not captured")
	}
	close(deferred.release)
	late := <-deferred.result
	assert.Error(t, late.err, "revoked candidate lease must reject the delayed transaction")

	response, err := sd.cli.Get(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	assert.Equal(t, int64(oldLeaseID), response.Kvs[0].Lease)
	assert.Equal(t, self.AsJSONString(), string(response.Kvs[0].Value))
}

func TestEtcdSDDelayedFlushCannotRebindRegistrationToOldLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, map[string]string{"version": "published"})
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	oldLeaseID := clientv3.LeaseID(sd.leaseID.Load())

	ambiguousErr := errors.New("ambiguous flush transaction")
	deferred := &deferredMutationKV{
		KV:      sd.cli.KV,
		err:     ambiguousErr,
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  make(chan deferredMutationResult, 1),
	}
	sd.cli.KV = deferred
	latePayload := *self
	latePayload.Metadata = map[string]string{"version": "late-old-lease"}
	assert.ErrorIs(t, sd.flushServerWithLeaseContext(context.Background(), &latePayload), ambiguousErr)
	select {
	case <-deferred.started:
	default:
		t.Fatal("flush transaction was not captured")
	}

	require.NoError(t, sd.renewLease(sd.lifecycleCtx, oldLeaseID))
	newLeaseID := sd.leaseID.Load()
	require.NotEqual(t, int64(oldLeaseID), newLeaseID)
	close(deferred.release)
	late := <-deferred.result
	require.NoError(t, late.err)
	require.NotNil(t, late.txnResponse)
	assert.False(t, late.txnResponse.Succeeded)

	response, err := sd.cli.Get(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	assert.Equal(t, newLeaseID, response.Kvs[0].Lease)
	assert.Equal(t, self.AsJSONString(), string(response.Kvs[0].Value))
}

func TestEtcdSDRenewalSerializesFlushLeasePublication(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	oldLeaseID := sd.leaseID.Load()

	baseKV := sd.cli.KV
	blockingKV := &postCommitBlockingPutKV{
		KV:             baseKV,
		firstCommitted: make(chan struct{}),
		secondStarted:  make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	sd.cli.KV = blockingKV
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	t.Cleanup(release)

	renewResult := make(chan error, 1)
	go func() { renewResult <- sd.renewLease(sd.lifecycleCtx, clientv3.LeaseID(oldLeaseID)) }()
	select {
	case <-blockingKV.firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("candidate lease registration did not commit")
	}

	flushInvoked := make(chan struct{})
	flushResult := make(chan error, 1)
	go func() {
		close(flushInvoked)
		flushResult <- sd.FlushServer2Cluster(self)
	}()
	<-flushInvoked
	select {
	case <-blockingKV.secondStarted:
		t.Fatal("FlushServer2Cluster reached etcd before candidate lease publication")
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case err := <-renewResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("lease renewal did not finish")
	}
	select {
	case err := <-flushResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("FlushServer2Cluster did not finish")
	}

	newLeaseID := sd.leaseID.Load()
	require.NotEqual(t, oldLeaseID, newLeaseID)
	response, err := baseKV.Get(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	assert.Equal(t, newLeaseID, response.Kvs[0].Lease)
}

func TestEtcdSDConcurrentFlushesSharePublishedLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	blockingKV := &concurrentBlockingPutKV{
		KV:            sd.cli.KV,
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	sd.cli.KV = blockingKV
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	t.Cleanup(release)

	first := *self
	first.Metadata = map[string]string{"flush": "first"}
	second := *self
	second.Metadata = map[string]string{"flush": "second"}
	firstResult := make(chan error, 1)
	go func() { firstResult <- sd.FlushServer2Cluster(&first) }()
	select {
	case <-blockingKV.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first FlushServer2Cluster did not enter etcd Put")
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- sd.FlushServer2Cluster(&second) }()
	select {
	case <-blockingKV.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second FlushServer2Cluster was serialized behind the first")
	}
	select {
	case err := <-secondResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("second FlushServer2Cluster did not finish")
	}
	release()
	select {
	case err := <-firstResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first FlushServer2Cluster did not finish")
	}
}

func TestEtcdSDRenewalUsesFinalConcurrentFlushCommit(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	baseKV := sd.cli.KV
	blockingKV := &postCommitBlockingPutKV{
		KV:             baseKV,
		firstCommitted: make(chan struct{}),
		secondStarted:  make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	sd.cli.KV = blockingKV
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	t.Cleanup(release)

	first := *self
	first.Metadata = map[string]string{"flush": "first"}
	second := *self
	second.Metadata = map[string]string{"flush": "second"}
	firstResult := make(chan error, 1)
	go func() { firstResult <- sd.FlushServer2Cluster(&first) }()
	select {
	case <-blockingKV.firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("first FlushServer2Cluster did not commit")
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- sd.FlushServer2Cluster(&second) }()
	select {
	case <-blockingKV.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second FlushServer2Cluster did not enter etcd Put")
	}
	select {
	case err := <-secondResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("second FlushServer2Cluster did not finish")
	}
	release()
	select {
	case err := <-firstResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first FlushServer2Cluster did not finish")
	}

	oldLeaseID := clientv3.LeaseID(sd.leaseID.Load())
	_, err := baseKV.Delete(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.NoError(t, sd.renewLease(sd.lifecycleCtx, oldLeaseID))
	response, err := baseKV.Get(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	registered, err := parseServer(response.Kvs[0].Value)
	require.NoError(t, err)
	assert.Equal(t, "second", registered.Metadata["flush"])
	assert.Equal(t, sd.leaseID.Load(), response.Kvs[0].Lease)
}

func TestEtcdSDFlushKeepsHistoricalUnboundedRequestUntilShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())
	sd.etcdDialTimeout = 100 * time.Millisecond

	blockingKV := &firstPutBlockingKV{
		KV:           sd.cli.KV,
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		deadline:     make(chan bool, 1),
	}
	sd.cli.KV = blockingKV
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	t.Cleanup(release)
	flushResult := make(chan error, 1)
	go func() { flushResult <- sd.FlushServer2Cluster(self) }()
	select {
	case hasDeadline := <-blockingKV.deadline:
		assert.False(t, hasDeadline, "DialTimeout must not change the historical Flush request deadline")
	case <-time.After(5 * time.Second):
		t.Fatal("FlushServer2Cluster did not reach its etcd transaction")
	}
	require.NoError(t, sd.Shutdown())
	select {
	case err := <-flushResult:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("FlushServer2Cluster did not observe shutdown cancellation")
	}
	assert.Equal(t, 1, blockingKV.calls())
}

func TestEtcdSDRenewalIsNotStarvedByBlockedFlush(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	blockingKV := &concurrentBlockingPutKV{
		KV:            sd.cli.KV,
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	sd.cli.KV = blockingKV
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	t.Cleanup(release)

	flushResult := make(chan error, 1)
	go func() { flushResult <- sd.FlushServer2Cluster(self) }()
	select {
	case <-blockingKV.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("FlushServer2Cluster did not enter its etcd Put")
	}

	oldLeaseID := clientv3.LeaseID(sd.leaseID.Load())
	renewalResult := make(chan error, 1)
	go func() { renewalResult <- sd.renewLease(sd.lifecycleCtx, oldLeaseID) }()
	select {
	case <-blockingKV.secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("lease renewal was starved behind the blocked Flush transaction")
	}
	select {
	case err := <-renewalResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("lease renewal did not complete after crossing the publication gate")
	}
	assert.NotEqual(t, int64(oldLeaseID), sd.leaseID.Load())
	release()
	select {
	case err := <-flushResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("blocked FlushServer2Cluster did not retry on the renewed lease")
	}
}

func TestEtcdSDLeaseActivationFailurePreservesPublishedLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), nil)
	sd.leaseID.Store(11)
	sd.lifecycleLock.Lock()
	sd.lifecycleState = etcdLifecycleStopping
	sd.lifecycleLock.Unlock()
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	t.Cleanup(cancelLease)

	releasePublication, err := sd.acquireLeasePublication(context.Background())
	require.NoError(t, err)
	published, err := sd.activateLeaseLocked(&leaseKeepAlive{
		id:        22,
		ctx:       leaseCtx,
		responses: make(chan *clientv3.LeaseKeepAliveResponse),
		cancel:    cancelLease,
	}, 11)
	releasePublication()
	assert.False(t, published)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(11), sd.leaseID.Load())
}

func TestEtcdSDRenewalDoesNotWaitForCallbackBackpressure(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.namespaceClient(sd.cli)
	sd.lifecycleLock.Lock()
	sd.lifecycleState = etcdLifecycleInitializing
	sd.lifecycleLock.Unlock()
	require.NoError(t, sd.grantLease(sd.leaseCtx))
	sd.lifecycleLock.Lock()
	sd.lifecycleState = etcdLifecycleRunning
	sd.lifecycleLock.Unlock()
	sd.running.Store(true)
	require.NoError(t, sd.bootstrapServerWithLeaseContext(sd.lifecycleCtx, self, clientv3.LeaseID(sd.leaseID.Load())))

	remote := NewServer("room-1", "room", false, nil)
	_, err := sd.cli.Put(context.Background(), getKey(remote.ID, remote.Type), remote.AsJSONString())
	require.NoError(t, err)
	sd.AddListener(&testSDListener{add: func(*Server) {}})

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted
	for i := 0; i < callbackQueueLimit; i++ {
		enqueueTestCallback(t, sd, nil)
	}

	renewResult := make(chan error, 1)
	go func() { renewResult <- sd.renewLease(sd.lifecycleCtx, clientv3.LeaseID(sd.leaseID.Load())) }()
	select {
	case err := <-renewResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("lease renewal waited for the saturated callback queue")
	}
	release()
	<-blockerDone
}

func TestEtcdBeforeShutdown(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			e.BeforeShutdown()
			require.NoError(t, e.Init())
			assert.True(t, e.Running())
			e.BeforeShutdown()
			assert.True(t, e.Running())
			_, err := cli.Revoke(context.TODO(), clientv3.LeaseID(e.leaseID.Load()))
			assert.Error(t, err)
		})
	}
}

func TestEtcdBeforeShutdownCancelsBlockedFlushBeforeRevoke(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.shutdownDelay = 0
	require.NoError(t, sd.Init())

	blockingKV := &firstPutBlockingKV{
		KV:           sd.cli.KV,
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	sd.cli.KV = blockingKV
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	t.Cleanup(release)
	flushResult := make(chan error, 1)
	callbackDone := enqueueTestCallback(t, sd, func() {
		flushResult <- sd.FlushServer2Cluster(self)
	})
	select {
	case <-blockingKV.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("FlushServer2Cluster did not reach its etcd transaction")
	}

	beforeShutdownDone := make(chan struct{})
	go func() {
		sd.BeforeShutdown()
		close(beforeShutdownDone)
	}()
	select {
	case <-beforeShutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("BeforeShutdown spent its revoke budget behind a blocked Flush")
	}
	select {
	case err := <-flushResult:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Flush did not observe BeforeShutdown cancellation")
	}
	select {
	case <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted callback did not finish after Flush cancellation")
	}
	response, err := sd.cli.Get(context.Background(), getKey(self.ID, self.Type))
	require.NoError(t, err)
	assert.Empty(t, response.Kvs, "published lease must be revoked before BeforeShutdown returns")
}

func TestEtcdBeforeShutdownWaitsForAcceptedCallbacks(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	sd.shutdownDelay = 0
	require.NoError(t, sd.Init())

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	callbackDone := enqueueTestCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	beforeShutdownDone := make(chan struct{})
	go func() {
		sd.BeforeShutdown()
		close(beforeShutdownDone)
	}()
	helpers.ShouldEventuallyReturn(t, func() bool {
		sd.callbackLock.Lock()
		defer sd.callbackLock.Unlock()
		return sd.callbackStopping
	}, true)
	select {
	case <-beforeShutdownDone:
		t.Fatal("BeforeShutdown returned before accepted callbacks drained")
	default:
	}

	release()
	select {
	case <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for accepted callback")
	}
	select {
	case <-beforeShutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for BeforeShutdown callback barrier")
	}
}

func TestEtcdBeforeShutdownDrainsAfterListenerShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	require.NoError(t, sd.Init())

	shutdownReturned := make(chan error, 1)
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	callbackDone := enqueueTestCallback(t, sd, func() {
		shutdownReturned <- sd.Shutdown()
		<-releaseCallback
	})
	select {
	case err := <-shutdownReturned:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener Shutdown did not return")
	}
	assert.False(t, sd.Running())

	beforeShutdownDone := make(chan struct{})
	go func() {
		sd.BeforeShutdown()
		close(beforeShutdownDone)
	}()
	select {
	case <-beforeShutdownDone:
		t.Fatal("BeforeShutdown returned before the listener callback exited")
	default:
	}

	release()
	select {
	case <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for listener callback")
	}
	select {
	case <-beforeShutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-Shutdown callback drain")
	}
}

func TestEtcdShutdown(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			assert.True(t, e.Running())
			require.NoError(t, e.Shutdown())
			require.NoError(t, e.Shutdown())
			assert.False(t, e.Running())
		})
	}
}

func TestEtcdElectionInitPublishesStateBeforeAsyncCallback(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 3 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.reconnectBackOff = 10 * time.Millisecond

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(release)
	blockerDone := enqueueTestLeaderCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	type observation struct {
		event    string
		isLeader bool
	}
	observations := make(chan observation, 1)
	sd.OnLeaderChange(func(leader string) {
		observations <- observation{event: leader, isLeader: sd.IsLeader()}
	})

	require.NoError(t, sd.Init())
	assert.Equal(t, self.ID, sd.LeaderID())
	assert.True(t, sd.IsLeader())
	select {
	case actual := <-observations:
		t.Fatalf("initial leader listener was not asynchronous: %+v", actual)
	default:
	}

	release()
	<-blockerDone
	select {
	case actual := <-observations:
		assert.Equal(t, self.ID, actual.event)
		assert.True(t, actual.isLeader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial leader listener")
	}
}

func TestEtcdElectionReconnectAndShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 3 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, sd.Init())
	assert.True(t, sd.IsLeader(), "Init must publish the initial no-leader campaign result")

	var previousLease clientv3.LeaseID
	for attempt := 0; attempt < 2; attempt++ {
		response, err := sd.cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
		require.NoError(t, err)
		require.Len(t, response.Kvs, 1)
		leaseID := clientv3.LeaseID(response.Kvs[0].Lease)
		if previousLease != 0 {
			require.NotEqual(t, previousLease, leaseID)
		}
		previousLease = leaseID
		_, err = cli.Revoke(context.Background(), leaseID)
		require.NoError(t, err)

		helpers.ShouldEventuallyReturn(t, func() bool {
			current, err := sd.cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
			return err == nil && len(current.Kvs) == 1 &&
				clientv3.LeaseID(current.Kvs[0].Lease) != leaseID && sd.IsLeader()
		}, true, 10*time.Millisecond, 5*time.Second)
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- sd.Shutdown() }()
	select {
	case err := <-shutdownDone:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for election shutdown")
	}
}

func TestEtcdElectionObserveClosurePublishesOrderedGeneration(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 3 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	leaderServer := NewServer("frontend-leader", "frontend", true, nil)
	leader := getEtcdSD(t, cfg, leaderServer, newTestClientView(context.Background(), cli))
	leader.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, leader.Init())
	assert.True(t, leader.IsLeader())

	closingWatcher := &closeFirstElectionWatcher{
		Watcher: cli.Watcher,
		closeAt: 2, // initial exact-key control watch is first; close Observe's watch
		done:    make(chan struct{}),
	}
	campaignGate := &firstPutBlockingKV{
		KV:           cli.KV,
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	var releaseGateOnce sync.Once
	// Registered before getEtcdSD so follower.Shutdown cancels Campaign before
	// this failsafe can release its first transaction.
	t.Cleanup(func() { releaseGateOnce.Do(func() { close(campaignGate.releaseFirst) }) })
	followerServer := NewServer("frontend-follower", "frontend", true, nil)
	followerClient := newTestClientView(context.Background(), cli)
	followerClient.KV = campaignGate
	followerClient.Watcher = closingWatcher
	follower := getEtcdSD(t, cfg, followerServer, followerClient)
	follower.reconnectBackOff = 10 * time.Millisecond
	type observation struct {
		event string
		state string
	}
	observations := make(chan observation, 4)
	follower.OnLeaderChange(func(leader string) {
		observations <- observation{event: leader, state: follower.LeaderID()}
	})

	require.NoError(t, follower.Init())
	select {
	case <-campaignGate.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for gated Campaign transaction")
	}
	select {
	case <-closingWatcher.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for injected Observe closure")
	}

	for _, expected := range []string{leaderServer.ID, "", leaderServer.ID} {
		select {
		case actual := <-observations:
			assert.Equal(t, expected, actual.event)
			assert.Equal(t, expected, actual.state)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for leader event %q", expected)
		}
	}
	helpers.ShouldEventuallyReturn(t, func() bool {
		response, getErr := follower.cli.Get(context.Background(), follower.electionName+"/", clientv3.WithPrefix())
		if getErr != nil {
			return false
		}
		for _, kv := range response.Kvs {
			if string(kv.Value) == followerServer.ID {
				return kv.Lease == follower.leaseID.Load()
			}
		}
		return false
	}, true, 10*time.Millisecond, 5*time.Second)
}

func TestEtcdElectionGenerationRetryPreservesLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 3 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	leaderServer := NewServer("frontend-leader", "frontend", true, nil)
	leader := getEtcdSD(t, cfg, leaderServer, newTestClientView(context.Background(), cli))
	leader.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, leader.Init())

	closingWatcher := &closeFirstElectionWatcher{
		Watcher: cli.Watcher,
		closeAt: 2, // initial exact-key control watch is first; close Observe's watch
		done:    make(chan struct{}),
	}
	campaignGate := &firstPutBlockingKV{
		KV:           cli.KV,
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	var releaseGateOnce sync.Once
	t.Cleanup(func() { releaseGateOnce.Do(func() { close(campaignGate.releaseFirst) }) })
	failingLease := &failKeepAliveCallLease{
		Lease:  cli.Lease,
		failAt: 3, // service keepalive, initial election session, then generation rebuild
		failed: make(chan struct{}, 1),
	}
	followerServer := NewServer("frontend-follower", "frontend", true, nil)
	followerClient := newTestClientView(context.Background(), cli)
	followerClient.KV = campaignGate
	followerClient.Lease = failingLease
	followerClient.Watcher = closingWatcher
	follower := getEtcdSD(t, cfg, followerServer, followerClient)
	follower.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, follower.Init())
	serviceLease := clientv3.LeaseID(follower.leaseID.Load())
	require.NotZero(t, serviceLease)

	select {
	case <-campaignGate.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the initial Campaign transaction")
	}
	select {
	case <-closingWatcher.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the injected Observe closure")
	}
	select {
	case <-failingLease.failed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out injecting the preserved-lease Session failure")
	}

	helpers.ShouldEventuallyReturn(t, func() bool {
		response, getErr := follower.cli.Get(context.Background(), follower.electionName+"/", clientv3.WithPrefix())
		if getErr != nil {
			return false
		}
		matches := 0
		for _, kv := range response.Kvs {
			if string(kv.Value) != followerServer.ID {
				continue
			}
			matches++
			if clientv3.LeaseID(kv.Lease) != serviceLease {
				return false
			}
		}
		return matches == 1
	}, true, 10*time.Millisecond, 5*time.Second)
}

func TestEtcdElectionCampaignLeaseFailureClassification(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	tests := []struct {
		name        string
		campaignErr error
		wantLease   bool
	}{
		{name: "transient", campaignErr: errors.New("injected transient campaign failure"), wantLease: true},
		{name: "lease not found", campaignErr: rpctypes.ErrLeaseNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			electionCtx, cancelElection := context.WithCancel(context.Background())
			t.Cleanup(cancelElection)
			generationCtx, cancelGeneration := context.WithCancel(electionCtx)
			generationClient := newTestClientView(generationCtx, cli)
			generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
				concurrency.WithContext(generationCtx))
			require.NoError(t, err)
			generationClient.KV = &failingPutKV{KV: cli.KV, err: tt.campaignErr}

			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			self := NewServer("frontend-"+strings.ReplaceAll(tt.name, " ", "-"), "frontend", true, nil)
			sd := getEtcdSD(t, cfg, self, cli)
			generation := &electionGeneration{
				token:    1,
				ctx:      generationCtx,
				cancel:   cancelGeneration,
				session:  generationSession,
				election: concurrency.NewElection(generationSession, sd.electionName),
			}
			t.Cleanup(func() { sd.stopElectionGeneration(generation, false) })

			type generationResult struct {
				lease     clientv3.LeaseID
				reconnect bool
			}
			result := make(chan generationResult, 1)
			go func() {
				lease, reconnect := sd.runElectionGeneration(electionCtx, generation, func(string) bool {
					t.Error("campaign failure unexpectedly published a leader")
					return true
				})
				result <- generationResult{lease: lease, reconnect: reconnect}
			}()

			select {
			case actual := <-result:
				assert.True(t, actual.reconnect)
				if tt.wantLease {
					assert.Equal(t, generationSession.Lease(), actual.lease)
				} else {
					assert.Zero(t, actual.lease)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out classifying Campaign failure")
			}
		})
	}
}

func TestEtcdElectionResumeResignLeaseFailureClassification(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	tests := []struct {
		name      string
		resignErr error
		wantLease bool
	}{
		{name: "transient", resignErr: errors.New("injected transient resign failure"), wantLease: true},
		{name: "lease not found", resignErr: rpctypes.ErrLeaseNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			electionCtx, cancelElection := context.WithCancel(context.Background())
			t.Cleanup(cancelElection)
			generationCtx, cancelGeneration := context.WithCancel(electionCtx)
			generationClient := newTestClientView(generationCtx, cli)
			generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
				concurrency.WithContext(generationCtx))
			require.NoError(t, err)

			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			self := NewServer("frontend-"+strings.ReplaceAll(tt.name, " ", "-"), "frontend", true, nil)
			sd := getEtcdSD(t, cfg, self, cli)
			sd.resumeLeader = false
			generationElection := concurrency.NewElection(generationSession, sd.electionName)
			require.NoError(t, generationElection.Campaign(context.Background(), self.ID))
			generationClient.KV = &failingPutKV{KV: cli.KV, err: tt.resignErr}
			generation := &electionGeneration{
				token:    1,
				ctx:      generationCtx,
				cancel:   cancelGeneration,
				session:  generationSession,
				election: generationElection,
			}
			t.Cleanup(func() { sd.stopElectionGeneration(generation, false) })

			type generationResult struct {
				lease     clientv3.LeaseID
				reconnect bool
			}
			result := make(chan generationResult, 1)
			go func() {
				lease, reconnect := sd.runElectionGeneration(electionCtx, generation, func(string) bool {
					t.Error("failed resume resignation unexpectedly published a leader")
					return true
				})
				result <- generationResult{lease: lease, reconnect: reconnect}
			}()

			select {
			case actual := <-result:
				assert.True(t, actual.reconnect)
				if tt.wantLease {
					assert.Equal(t, generationSession.Lease(), actual.lease)
				} else {
					assert.Zero(t, actual.lease)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out classifying resume Resign failure")
			}
		})
	}
}

func TestEtcdElectionShutdownRevokesDetachedCandidateLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 10 * time.Second
	cfg.Revoke.Timeout = time.Second
	cfg.Shutdown.Delay = 0
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	leader := getEtcdSD(t, cfg, NewServer("frontend-leader", "frontend", true, nil),
		newTestClientView(context.Background(), cli))
	leader.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, leader.Init())

	followerServer := NewServer("frontend-follower", "frontend", true, nil)
	follower := getEtcdSD(t, cfg, followerServer, newTestClientView(context.Background(), cli))
	follower.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, follower.Init())
	initialServiceLease := clientv3.LeaseID(follower.leaseID.Load())
	require.NotZero(t, initialServiceLease)
	_, err := cli.Revoke(context.Background(), initialServiceLease)
	require.NoError(t, err)
	require.NoError(t, follower.renewLease(follower.lifecycleCtx, initialServiceLease))

	var detachedElectionLease clientv3.LeaseID
	helpers.ShouldEventuallyReturn(t, func() bool {
		serviceLease := clientv3.LeaseID(follower.leaseID.Load())
		if serviceLease == 0 || serviceLease == initialServiceLease {
			return false
		}
		response, getErr := follower.cli.Get(context.Background(), follower.electionName+"/", clientv3.WithPrefix())
		if getErr != nil {
			return false
		}
		for _, kv := range response.Kvs {
			if string(kv.Value) != followerServer.ID {
				continue
			}
			detachedElectionLease = clientv3.LeaseID(kv.Lease)
			return detachedElectionLease != 0 && detachedElectionLease != serviceLease
		}
		return false
	}, true, 10*time.Millisecond, 5*time.Second)

	follower.BeforeShutdown()
	require.NoError(t, follower.Shutdown())
	response, err := follower.cli.Get(context.Background(), follower.electionName+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	for _, kv := range response.Kvs {
		assert.NotEqual(t, followerServer.ID, string(kv.Value),
			"shutdown left candidate lease %x eligible for promotion", detachedElectionLease)
	}
	ttl, err := cli.TimeToLive(context.Background(), detachedElectionLease)
	require.NoError(t, err)
	assert.Less(t, ttl.TTL, int64(0), "shutdown left detached election lease %x granted", detachedElectionLease)
}

func TestEtcdElectionShutdownDoesNotRevokeBorrowedLease(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Prefix = ""
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 10 * time.Second
	cfg.Revoke.Timeout = time.Second
	cfg.Shutdown.Delay = 0
	embedded, ownerClient := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	participantClient, err := clientv3.New(clientv3.Config{
		Endpoints:   ownerClient.Endpoints(),
		DialTimeout: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, participantClient.Close()) })

	self := NewServer("frontend-shared-id", "frontend", true, nil)
	ownerSession, err := concurrency.NewSession(ownerClient, concurrency.WithTTL(10))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ownerSession.Close() })
	ownerElection := concurrency.NewElection(ownerSession, "election/frontend")
	require.NoError(t, ownerElection.Campaign(context.Background(), self.ID))
	canaryKey := "borrowed-election-lease-canary"
	_, err = ownerClient.Put(context.Background(), canaryKey, "alive", clientv3.WithLease(ownerSession.Lease()))
	require.NoError(t, err)

	participant := getEtcdSD(t, cfg, self, participantClient)
	participant.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, participant.Init())
	require.NotEqual(t, ownerSession.Lease(), clientv3.LeaseID(participant.leaseID.Load()))
	helpers.ShouldEventuallyReturn(t, participant.IsLeader, true, 10*time.Millisecond, 5*time.Second)

	participant.BeforeShutdown()
	require.NoError(t, participant.Shutdown())
	ttl, err := ownerClient.TimeToLive(context.Background(), ownerSession.Lease())
	require.NoError(t, err)
	assert.Greater(t, ttl.TTL, int64(0), "participant revoked the adopted owner lease")
	canary, err := ownerClient.Get(context.Background(), canaryKey)
	require.NoError(t, err)
	require.Len(t, canary.Kvs, 1)
	_, err = ownerClient.Put(context.Background(), canaryKey+"-after-shutdown", "alive",
		clientv3.WithLease(ownerSession.Lease()))
	require.NoError(t, err)
	select {
	case <-ownerSession.Done():
		t.Fatal("participant closed the owner session by revoking its lease")
	default:
	}
}

func TestEtcdElectionCoordinatorRevokesPreservedLocalLeaseWhenStoppedDuringBackoff(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Heartbeat.TTL = 10 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	sd.reconnectBackOff = time.Minute
	electionCtx, cancelElection := context.WithCancel(context.Background())
	t.Cleanup(cancelElection)
	generation, err := sd.newElectionGeneration(electionCtx,
		electionLease{provenance: electionLeaseLocal}, 1)
	require.NoError(t, err)
	require.True(t, sd.activateLeaderGeneration(1))
	localLease := generation.session.Lease()
	require.NoError(t, generation.election.Campaign(context.Background(), sd.server.ID))
	electionKey := generation.election.Key()
	require.NotEmpty(t, electionKey)
	generationClient := generation.session.Client()
	generationClient.KV = &failingGetKV{
		KV:  generationClient.KV,
		err: errors.New("force preserved local lease reconnect"),
	}

	done := make(chan struct{})
	go func() {
		sd.runElectionCoordinator(electionCtx, generation, make(chan struct{}))
		close(done)
	}()
	helpers.ShouldEventuallyReturn(t, func() uint64 {
		sd.leaderAdmissionLock.Lock()
		defer sd.leaderAdmissionLock.Unlock()
		return sd.leaderGeneration
	}, uint64(2), 10*time.Millisecond, 5*time.Second)
	cancelElection()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop during reconnect backoff")
	}

	ttl, err := cli.TimeToLive(context.Background(), localLease)
	require.NoError(t, err)
	assert.Less(t, ttl.TTL, int64(0), "owned detached lease was not revoked on coordinator exit")
	candidate, err := cli.Get(context.Background(), electionKey)
	require.NoError(t, err)
	assert.Empty(t, candidate.Kvs, "owned detached election leader survived coordinator exit")
}

func TestEtcdElectionCoordinatorRevokesPreservedLocalLeaseWhenStoppedDuringSessionRetry(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Heartbeat.TTL = 10 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	sd.reconnectBackOff = 100 * time.Millisecond
	releaseKeepAlive := make(chan struct{})
	var releaseKeepAliveOnce sync.Once
	releaseFailedKeepAlive := func() { releaseKeepAliveOnce.Do(func() { close(releaseKeepAlive) }) }
	t.Cleanup(releaseFailedKeepAlive)
	failingLease := &failKeepAliveCallLease{
		Lease:   sd.cli.Lease,
		failAt:  2,
		failed:  make(chan struct{}, 1),
		release: releaseKeepAlive,
	}
	sd.cli.Lease = failingLease
	electionCtx, cancelElection := context.WithCancel(context.Background())
	t.Cleanup(cancelElection)
	generation, err := sd.newElectionGeneration(electionCtx,
		electionLease{provenance: electionLeaseLocal}, 1)
	require.NoError(t, err)
	require.True(t, sd.activateLeaderGeneration(1))
	localLease := generation.session.Lease()
	require.NoError(t, generation.election.Campaign(context.Background(), sd.server.ID))
	electionKey := generation.election.Key()
	require.NotEmpty(t, electionKey)
	generationClient := generation.session.Client()
	generationClient.KV = &failingGetKV{
		KV:  generationClient.KV,
		err: errors.New("force preserved local lease reconnect"),
	}

	done := make(chan struct{})
	go func() {
		sd.runElectionCoordinator(electionCtx, generation, make(chan struct{}))
		close(done)
	}()
	select {
	case <-failingLease.failed:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not attempt to reattach the preserved local lease")
	}
	cancelElection()
	releaseFailedKeepAlive()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop during session retry")
	}

	ttl, err := cli.TimeToLive(context.Background(), localLease)
	require.NoError(t, err)
	assert.Less(t, ttl.TTL, int64(0), "owned detached lease was not revoked on session retry exit")
	candidate, err := cli.Get(context.Background(), electionKey)
	require.NoError(t, err)
	assert.Empty(t, candidate.Kvs, "owned detached election leader survived session retry exit")
}

func TestEtcdElectionClearPrecedesDetachedLeaseRevoke(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Heartbeat.TTL = 10 * time.Second
	cfg.Revoke.Timeout = 5 * time.Second
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.reconnectBackOff = 10 * time.Millisecond

	electionCtx, cancelElection := context.WithCancel(context.Background())
	generation, err := sd.newElectionGeneration(electionCtx, electionLease{provenance: electionLeaseLocal}, 1)
	require.NoError(t, err)
	localLease := generation.lease.id
	require.NotZero(t, localLease)
	require.NoError(t, generation.election.Campaign(context.Background(), self.ID))
	selfKey := generation.election.Key()
	require.NotEmpty(t, selfKey)
	// Re-enter through the reconnect path so the generation resumes the seeded key.
	generation.election = concurrency.NewElection(generation.session, sd.electionName)
	require.True(t, sd.activateLeaderGeneration(generation.token))

	leaders := make(chan string, 3)
	sd.OnLeaderChange(func(leader string) { leaders <- leader })
	leaderReady := make(chan struct{})
	coordinatorDone := make(chan struct{})
	go func() {
		sd.runElectionCoordinator(electionCtx, generation, leaderReady)
		close(coordinatorDone)
	}()

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseCallbackOnce sync.Once
	releaseLeaderCallback := func() { releaseCallbackOnce.Do(func() { close(releaseCallback) }) }
	var releaseRevoke func()
	var foreignSession *concurrency.Session
	coordinatorStopped := false
	t.Cleanup(func() {
		cancelElection()
		releaseLeaderCallback()
		if releaseRevoke != nil {
			releaseRevoke()
		}
		if !coordinatorStopped {
			select {
			case <-coordinatorDone:
			case <-time.After(5 * time.Second):
				t.Error("timed out cleaning up election coordinator")
			}
		}
		if foreignSession != nil {
			_ = foreignSession.Close()
		}
	})

	select {
	case <-leaderReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial leader publication")
	}
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial leader callback")
	}
	blockerDone := enqueueTestLeaderCallback(t, sd, func() {
		close(callbackStarted)
		<-releaseCallback
	})
	<-callbackStarted

	baseLease := sd.cli.Lease
	blockingLease := &blockingRevokeLease{
		Lease:   baseLease,
		started: make(chan clientv3.LeaseID, 1),
		release: make(chan struct{}),
	}
	var releaseRevokeOnce sync.Once
	releaseRevoke = func() { releaseRevokeOnce.Do(func() { close(blockingLease.release) }) }
	sd.cli.Lease = blockingLease

	foreignSession, err = concurrency.NewSession(cli, concurrency.WithTTL(10))
	require.NoError(t, err)
	foreignLease := foreignSession.Lease()
	require.NotEqual(t, localLease, foreignLease)
	canaryKey := "foreign-election-lease-canary"
	_, err = cli.Put(context.Background(), canaryKey, "alive", clientv3.WithLease(foreignLease))
	require.NoError(t, err)
	// Rebinding the key is visible to Observe, while the exact-key control watch
	// filters the PUT and therefore cannot admit the clear under test.
	_, err = cli.Put(context.Background(), selfKey, self.ID, clientv3.WithLease(foreignLease))
	require.NoError(t, err)

	select {
	case revokedLease := <-blockingLease.started:
		require.Equal(t, localLease, revokedLease)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for owned lease cleanup")
	}
	sd.leaderAdmissionLock.Lock()
	generationWhileRevokeBlocked := sd.leaderGeneration
	sd.leaderAdmissionLock.Unlock()
	require.Equal(t, uint64(2), generationWhileRevokeBlocked)
	sd.leaderCallbackLock.Lock()
	queuedWhileRevokeBlocked := len(sd.leaderCallbackQueue)
	sd.leaderCallbackLock.Unlock()
	require.Equal(t, 1, queuedWhileRevokeBlocked, "clear was not admitted before remote cleanup")
	assert.True(t, sd.IsLeader(), "blocked callback should retain the prior applied state")

	current, err := cli.Get(context.Background(), selfKey)
	require.NoError(t, err)
	require.Len(t, current.Kvs, 1)
	require.Equal(t, foreignLease, clientv3.LeaseID(current.Kvs[0].Lease))
	localTTL, err := cli.TimeToLive(context.Background(), localLease)
	require.NoError(t, err)
	require.Greater(t, localTTL.TTL, int64(0), "cleanup reached the server before release")

	releaseLeaderCallback()
	<-blockerDone
	select {
	case leader := <-leaders:
		require.Empty(t, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out applying the admitted clear")
	}
	assert.False(t, sd.IsLeader())

	releaseRevoke()
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out resuming the borrowed leader lease")
	}
	helpers.ShouldEventuallyReturn(t, func() bool {
		ttl, ttlErr := cli.TimeToLive(context.Background(), localLease)
		return ttlErr == nil && ttl.TTL < 0
	}, true, 10*time.Millisecond, 5*time.Second)
	foreignTTL, err := cli.TimeToLive(context.Background(), foreignLease)
	require.NoError(t, err)
	assert.Greater(t, foreignTTL.TTL, int64(0))
	canary, err := cli.Get(context.Background(), canaryKey)
	require.NoError(t, err)
	require.Len(t, canary.Kvs, 1)

	cancelElection()
	select {
	case <-coordinatorDone:
		coordinatorStopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping election coordinator")
	}
}

func TestEtcdElectionCampaignShutdownUsesGenerationContext(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 3 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	leaderClient := newTestClientView(context.Background(), cli)
	leader := getEtcdSD(t, cfg, NewServer("frontend-leader", "frontend", true, nil), leaderClient)
	leader.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, leader.Init())
	helpers.ShouldEventuallyReturn(t, leader.IsLeader, true, 10*time.Millisecond, 5*time.Second)

	blocker := &cleanupBlockingKV{
		KV:                cli.KV,
		campaignCommitted: make(chan struct{}),
		cleanupStarted:    make(chan struct{}),
		releaseCleanup:    make(chan struct{}),
	}
	var releaseOnce sync.Once
	followerClient := newTestClientView(context.Background(), cli)
	followerClient.KV = blocker
	follower := getEtcdSD(t, cfg, NewServer("frontend-follower", "frontend", true, nil), followerClient)
	t.Cleanup(func() { releaseOnce.Do(func() { close(blocker.releaseCleanup) }) })
	follower.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, follower.Init())
	assert.False(t, follower.IsLeader())

	select {
	case <-blocker.campaignCommitted:
	case <-time.After(5 * time.Second):
		releaseOnce.Do(func() { close(blocker.releaseCleanup) })
		t.Fatal("timed out waiting for follower campaign")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- follower.Shutdown() }()
	select {
	case <-blocker.cleanupStarted:
	case <-time.After(5 * time.Second):
		releaseOnce.Do(func() { close(blocker.releaseCleanup) })
		select {
		case <-shutdownDone:
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown remained blocked before campaign cleanup")
		}
		t.Fatal("timed out waiting for campaign cleanup")
	}

	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		releaseOnce.Do(func() { close(blocker.releaseCleanup) })
		select {
		case <-shutdownDone:
		case <-time.After(5 * time.Second):
			t.Fatal("campaign cleanup remained blocked after test release")
		}
		t.Fatal("shutdown waited on Campaign cleanup using the injected client context")
	}
}

func TestEtcdElectionResumeKeepsPrefixBoundary(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	canarySession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, canarySession.Close()) })
	canaryElection := concurrency.NewElection(canarySession, "election/frontend-canary")
	require.NoError(t, canaryElection.Campaign(context.Background(), "frontend-canary"))

	self := NewServer("frontend-self", "frontend", true, nil)
	selfSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, selfSession.Close()) })
	selfElection := concurrency.NewElection(selfSession, "election/frontend")
	require.NoError(t, selfElection.Campaign(context.Background(), self.ID))
	initialLeader, err := selfElection.Leader(context.Background())
	require.NoError(t, err)
	require.Len(t, initialLeader.Kvs, 1)
	initialRevision := initialLeader.Kvs[0].CreateRevision

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  selfSession,
		election: concurrency.NewElection(selfSession, sd.electionName),
	}
	leaders := make(chan string, 1)
	result := make(chan bool, 1)
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case leader := <-leaders:
		assert.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for resumed election observation")
	}
	currentLeader, err := selfElection.Leader(context.Background())
	require.NoError(t, err)
	require.Len(t, currentLeader.Kvs, 1)
	assert.Equal(t, initialRevision, currentLeader.Kvs[0].CreateRevision)
	cancelGeneration()
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping resumed election generation")
	}
}

func TestEtcdElectionResumedLeaderDeletionWithoutSuccessorRecampaigns(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	self := NewServer("frontend-self", "frontend", true, nil)
	selfSession, err := concurrency.NewSession(cli, concurrency.WithTTL(10))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, selfSession.Close()) })
	selfElection := concurrency.NewElection(selfSession, "election/frontend")
	require.NoError(t, selfElection.Campaign(context.Background(), self.ID))

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  selfSession,
		election: concurrency.NewElection(selfSession, sd.electionName),
	}
	leaders := make(chan string, 3)
	result := make(chan bool, 1)
	generationStopped := false
	t.Cleanup(func() {
		if generationStopped {
			return
		}
		cancelGeneration()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("timed out cleaning up resumed election generation")
		}
	})
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for resumed self publication")
	}
	response, err := cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	selfKey := string(response.Kvs[0].Key)
	initialCreateRevision := response.Kvs[0].CreateRevision
	require.Equal(t, selfSession.Lease(), clientv3.LeaseID(response.Kvs[0].Lease))
	_, err = cli.Delete(context.Background(), selfKey)
	require.NoError(t, err)
	ttl, err := cli.TimeToLive(context.Background(), selfSession.Lease())
	require.NoError(t, err)
	require.Greater(t, ttl.TTL, int64(0))

	select {
	case leader := <-leaders:
		require.Empty(t, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("leader deletion without a successor did not clear local state")
	}
	helpers.ShouldEventuallyReturn(t, func() bool {
		response, getErr := cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
		if getErr != nil || len(response.Kvs) != 1 {
			return false
		}
		return string(response.Kvs[0].Key) == selfKey && string(response.Kvs[0].Value) == self.ID &&
			clientv3.LeaseID(response.Kvs[0].Lease) == selfSession.Lease() &&
			response.Kvs[0].CreateRevision > initialCreateRevision
	}, true, 10*time.Millisecond, 5*time.Second)
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("deleted resumed leader did not recampaign with the live lease")
	}

	cancelGeneration()
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
		generationStopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping resumed election generation")
	}
}

func TestEtcdElectionDelayedObserveDoesNotResurrectDeletedLeader(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	const prefix = "election/frontend"
	leaderSession, err := concurrency.NewSession(cli, concurrency.WithTTL(10))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, leaderSession.Close()) })
	leaderElection := concurrency.NewElection(leaderSession, prefix)
	require.NoError(t, leaderElection.Campaign(context.Background(), "frontend-a"))

	replacementSession, err := concurrency.NewSession(cli, concurrency.WithTTL(10))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, replacementSession.Close()) })
	replacementElection := concurrency.NewElection(replacementSession, prefix)
	replacementCampaignDone := make(chan error, 1)
	go func() {
		replacementCampaignDone <- replacementElection.Campaign(context.Background(), "frontend-b")
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)

	seed, err := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	var leaderA, leaderB *mvccpb.KeyValue
	for _, kv := range seed.Kvs {
		switch string(kv.Value) {
		case "frontend-a":
			leaderA = kv
		case "frontend-b":
			leaderB = kv
		}
	}
	require.NotNil(t, leaderA)
	require.NotNil(t, leaderB)

	observeResponses := make(chan clientv3.WatchResponse, 1)
	delayedWatcher := &delayedElectionObserveWatcher{
		Watcher:                 cli.Watcher,
		leaderKey:               string(leaderA.Key),
		replacementKey:          string(leaderB.Key),
		observeStarted:          make(chan struct{}),
		replacementWatchStarted: make(chan struct{}),
		successorWatchStarted:   make(chan struct{}),
		observeResponses:        observeResponses,
	}
	campaignBlocker := &postCommitBlockingPutKV{
		KV:             cli.KV,
		firstCommitted: make(chan struct{}),
		secondStarted:  make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	generationClient := newTestClientView(generationCtx, cli)
	generationClient.KV = campaignBlocker
	generationClient.Watcher = delayedWatcher
	generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(10),
		concurrency.WithContext(generationCtx))
	require.NoError(t, err)
	t.Cleanup(generationSession.Orphan)
	delayedWatcher.successorKey = fmt.Sprintf("%s/%x", prefix, generationSession.Lease())

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  generationSession,
		election: concurrency.NewElection(generationSession, sd.electionName),
	}
	leaders := make(chan string, 3)
	result := make(chan bool, 1)
	generationStopped := false
	var closeObserveOnce sync.Once
	closeObserve := func() { closeObserveOnce.Do(func() { close(observeResponses) }) }
	t.Cleanup(func() {
		closeObserve()
		cancelGeneration()
		if generationStopped {
			return
		}
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("timed out cleaning up delayed Observe generation")
		}
	})
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case leader := <-leaders:
		require.Equal(t, "frontend-a", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the initial leader")
	}
	select {
	case <-delayedWatcher.observeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out installing the delayed Observe watch")
	}
	select {
	case <-campaignBlocker.firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out committing the blocked local candidate")
	}
	initialLeaderAModRevision := leaderA.ModRevision
	require.NoError(t, leaderElection.Proclaim(context.Background(), "frontend-a"))
	delayedPut, err := cli.Get(context.Background(), string(leaderA.Key))
	require.NoError(t, err)
	require.Len(t, delayedPut.Kvs, 1)
	delayedLeaderA := delayedPut.Kvs[0]
	require.Equal(t, leaderA.CreateRevision, delayedLeaderA.CreateRevision)
	require.Greater(t, delayedLeaderA.ModRevision, initialLeaderAModRevision)

	deleteA, err := cli.Delete(context.Background(), string(leaderA.Key))
	require.NoError(t, err)
	require.NotNil(t, deleteA.Header)
	select {
	case campaignErr := <-replacementCampaignDone:
		require.NoError(t, campaignErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out promoting replacement leader B")
	}
	select {
	case <-delayedWatcher.replacementWatchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not consume leader A deletion")
	}

	deleteB, err := cli.Delete(context.Background(), string(leaderB.Key))
	require.NoError(t, err)
	require.NotNil(t, deleteB.Header)
	select {
	case <-delayedWatcher.successorWatchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not consume leader B deletion")
	}

	observeResponses <- clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: deleteB.Header.Revision},
		Events: []*clientv3.Event{
			{
				Type: clientv3.EventTypePut,
				Kv: &mvccpb.KeyValue{
					Key:            append([]byte(nil), delayedLeaderA.Key...),
					Value:          append([]byte(nil), delayedLeaderA.Value...),
					CreateRevision: delayedLeaderA.CreateRevision,
					ModRevision:    delayedLeaderA.ModRevision,
					Lease:          delayedLeaderA.Lease,
				},
			},
			{
				Type: clientv3.EventTypeDelete,
				Kv: &mvccpb.KeyValue{
					Key:            append([]byte(nil), delayedLeaderA.Key...),
					CreateRevision: delayedLeaderA.CreateRevision,
					ModRevision:    deleteA.Header.Revision,
				},
			},
		},
	}

	var nextLeader string
	select {
	case nextLeader = <-leaders:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the current successor")
	}
	cancelGeneration()
	closeObserve()
	select {
	case <-result:
		generationStopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping delayed Observe generation")
	}
	assert.Equal(t, "frontend-self", nextLeader,
		"republished deleted leader from delayed Observe response")
}

func TestEtcdElectionLeaderRecheckUsesBoundedSessionContext(t *testing.T) {
	for _, test := range []struct {
		name        string
		readTimeout time.Duration
		stopSession bool
		wantLease   bool
	}{
		{name: "session stops", readTimeout: 5 * time.Second, stopSession: true},
		{name: "read times out", readTimeout: 200 * time.Millisecond, wantLease: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			embedded, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { embedded.Terminate(t) })

			const prefix = "election/frontend"
			leaderSession, err := concurrency.NewSession(cli, concurrency.WithTTL(10))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, leaderSession.Close()) })
			leaderElection := concurrency.NewElection(leaderSession, prefix)
			require.NoError(t, leaderElection.Campaign(context.Background(), "frontend-leader"))

			electionCtx, cancelElection := context.WithCancel(context.Background())
			generationCtx, cancelGeneration := context.WithCancel(electionCtx)
			blockingKV := &blockBoundedElectionGetKV{
				KV:      cli.KV,
				blockAt: 2,
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			generationClient := newTestClientView(generationCtx, cli)
			generationClient.KV = blockingKV
			generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(10),
				concurrency.WithContext(generationCtx))
			require.NoError(t, err)
			generationLease := generationSession.Lease()

			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			cfg.Revoke.Timeout = test.readTimeout
			sd := getEtcdSD(t, cfg, NewServer("frontend-self", "frontend", true, nil), cli)
			generation := &electionGeneration{
				token:    1,
				ctx:      generationCtx,
				cancel:   cancelGeneration,
				session:  generationSession,
				election: concurrency.NewElection(generationSession, sd.electionName),
			}
			type generationResult struct {
				lease     clientv3.LeaseID
				reconnect bool
			}
			published := make(chan string, 1)
			result := make(chan generationResult, 1)
			generationStopped := false
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(blockingKV.release) }) }
			t.Cleanup(func() {
				release()
				cancelElection()
				if generationStopped {
					return
				}
				select {
				case <-result:
				case <-time.After(5 * time.Second):
					t.Error("timed out cleaning up blocked leader recheck")
				}
			})
			go func() {
				lease, reconnect := sd.runElectionGeneration(electionCtx, generation, func(leader string) bool {
					published <- leader
					return true
				})
				result <- generationResult{lease: lease, reconnect: reconnect}
			}()

			select {
			case <-blockingKV.started:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out blocking the bounded leader identity recheck")
			}
			if test.stopSession {
				generationSession.Orphan()
			}

			select {
			case actual := <-result:
				if test.wantLease {
					assert.Equal(t, generationLease, actual.lease)
				} else {
					assert.Zero(t, actual.lease)
				}
				assert.True(t, actual.reconnect)
				assert.NoError(t, electionCtx.Err())
				generationStopped = true
			case <-time.After(2 * time.Second):
				release()
				select {
				case <-result:
					generationStopped = true
				case <-time.After(5 * time.Second):
					t.Fatal("leader recheck remained blocked after test release")
				}
				t.Fatal("leader recheck did not stop at its bounded context")
			}
			select {
			case leader := <-published:
				t.Fatalf("published leader before completing the blocked identity recheck: %q", leader)
			default:
			}
		})
	}
}

func TestEtcdElectionResumeDisabledRecampaigns(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	self := NewServer("frontend-self", "frontend", true, nil)
	selfSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, selfSession.Close()) })
	selfElection := concurrency.NewElection(selfSession, "election/frontend")
	require.NoError(t, selfElection.Campaign(context.Background(), self.ID))
	initialLeader, err := selfElection.Leader(context.Background())
	require.NoError(t, err)
	require.Len(t, initialLeader.Kvs, 1)
	initialRevision := initialLeader.Kvs[0].CreateRevision

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	sd.resumeLeader = false
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  selfSession,
		election: concurrency.NewElection(selfSession, sd.electionName),
	}
	leaders := make(chan string, 1)
	result := make(chan bool, 1)
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case leader := <-leaders:
		assert.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recampaign observation")
	}
	currentLeader, err := selfElection.Leader(context.Background())
	require.NoError(t, err)
	require.Len(t, currentLeader.Kvs, 1)
	assert.Greater(t, currentLeader.Kvs[0].CreateRevision, initialRevision)
	cancelGeneration()
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping recampaign generation")
	}
}

func TestEtcdElectionResumeSnapshotChangeRecampaigns(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	self := NewServer("frontend-self", "frontend", true, nil)
	selfSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, selfSession.Close()) })
	selfElection := concurrency.NewElection(selfSession, "election/frontend")
	require.NoError(t, selfElection.Campaign(context.Background(), self.ID))

	otherSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherSession.Close()) })
	otherElection := concurrency.NewElection(otherSession, "election/frontend")
	otherCampaignDone := make(chan error, 1)
	go func() {
		otherCampaignDone <- otherElection.Campaign(context.Background(), "frontend-other")
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), "election/frontend/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)

	blockingKV := &blockElectionGetKV{
		KV:      cli.KV,
		blockAt: 1,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockingKV.release) }) }
	t.Cleanup(release)
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	blockingKV.controlCtx = generationCtx
	generationClient := newTestClientView(generationCtx, cli)
	generationClient.KV = blockingKV
	generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
		concurrency.WithContext(generationCtx), concurrency.WithLease(selfSession.Lease()))
	require.NoError(t, err)
	t.Cleanup(generationSession.Orphan)

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  generationSession,
		election: concurrency.NewElection(generationSession, sd.electionName),
	}
	leaders := make(chan string, 2)
	result := make(chan bool, 1)
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			if leader == self.ID {
				cancelGeneration()
			}
			return true
		})
		result <- reconnect
	}()

	select {
	case <-blockingKV.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting between resume snapshot and Observe get")
	}
	require.NoError(t, selfElection.Resign(context.Background()))
	select {
	case campaignErr := <-otherCampaignDone:
		require.NoError(t, campaignErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out promoting replacement leader")
	}
	release()

	select {
	case leader := <-leaders:
		assert.Equal(t, "frontend-other", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing replacement leader")
	}
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), "election/frontend/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)
	require.NoError(t, otherElection.Resign(context.Background()))
	select {
	case leader := <-leaders:
		assert.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing resumed campaign")
	}
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping resumed generation")
	}
}

func TestEtcdElectionObservedSelfRequiresGenerationLease(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	prefix := "election/frontend"
	leaderSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, leaderSession.Close()) })
	leaderElection := concurrency.NewElection(leaderSession, prefix)
	require.NoError(t, leaderElection.Campaign(context.Background(), "frontend-other"))

	self := NewServer("frontend-self", "frontend", true, nil)
	staleSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staleSession.Close()) })
	staleElection := concurrency.NewElection(staleSession, prefix)
	staleCampaignCtx, cancelStaleCampaign := context.WithCancel(context.Background())
	t.Cleanup(cancelStaleCampaign)
	staleCampaignDone := make(chan error, 1)
	go func() {
		staleCampaignDone <- staleElection.Campaign(staleCampaignCtx, self.ID)
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)

	electionCtx, cancelElection := context.WithCancel(context.Background())
	t.Cleanup(cancelElection)
	generationCtx, cancelGeneration := context.WithCancel(electionCtx)
	generationClient := newTestClientView(generationCtx, cli)
	generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
		concurrency.WithContext(generationCtx))
	require.NoError(t, err)
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  generationSession,
		election: concurrency.NewElection(generationSession, sd.electionName),
	}
	t.Cleanup(func() { sd.stopElectionGeneration(generation, false) })

	leaders := make(chan string, 2)
	type generationResult struct {
		lease     clientv3.LeaseID
		reconnect bool
	}
	result := make(chan generationResult, 1)
	go func() {
		lease, reconnect := sd.runElectionGeneration(electionCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- generationResult{lease: lease, reconnect: reconnect}
	}()

	select {
	case leader := <-leaders:
		require.Equal(t, "frontend-other", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the original leader")
	}
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 3, 10*time.Millisecond, 5*time.Second)

	require.NoError(t, leaderElection.Resign(context.Background()))
	select {
	case campaignErr := <-staleCampaignDone:
		require.NoError(t, campaignErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out promoting the stale same-ID candidate")
	}

	select {
	case actual := <-result:
		assert.True(t, actual.reconnect)
		assert.Equal(t, staleSession.Lease(), actual.lease)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out switching to the observed same-ID candidate lease")
	}
	select {
	case published := <-leaders:
		t.Fatalf("published same-ID candidate before adopting its lease: %q", published)
	default:
	}
}

func TestEtcdElectionForeignObservationBeforeCampaignResultRecampaigns(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	prefix := "election/frontend"
	self := NewServer("frontend-self", "frontend", true, nil)
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	blockingGet := &blockElectionGetKV{
		KV:      cli.KV,
		blockAt: 1,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	blockingCampaign := &postCommitBlockingPutKV{
		KV:             blockingGet,
		firstCommitted: make(chan struct{}),
		secondStarted:  make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	var releaseGetOnce sync.Once
	releaseGet := func() { releaseGetOnce.Do(func() { close(blockingGet.release) }) }
	var releaseCampaignOnce sync.Once
	releaseCampaign := func() { releaseCampaignOnce.Do(func() { close(blockingCampaign.releaseFirst) }) }
	blockingGet.controlCtx = generationCtx
	generationClient := newTestClientView(generationCtx, cli)
	generationClient.KV = blockingCampaign
	generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
		concurrency.WithContext(generationCtx))
	require.NoError(t, err)
	t.Cleanup(generationSession.Orphan)

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  generationSession,
		election: concurrency.NewElection(generationSession, sd.electionName),
	}
	leaders := make(chan string, 3)
	result := make(chan bool, 1)
	generationStopped := false
	t.Cleanup(func() {
		if generationStopped {
			return
		}
		cancelGeneration()
		releaseGet()
		releaseCampaign()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("timed out cleaning up election generation")
		}
	})
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case <-blockingGet.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out blocking the first Observe snapshot")
	}
	select {
	case <-blockingCampaign.firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first Campaign commit")
	}

	otherSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherSession.Close()) })
	otherElection := concurrency.NewElection(otherSession, prefix)
	otherCampaignDone := make(chan error, 1)
	go func() {
		otherCampaignDone <- otherElection.Campaign(context.Background(), "frontend-other")
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)

	response, err := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	var selfKey string
	for _, kv := range response.Kvs {
		if string(kv.Value) == self.ID {
			selfKey = string(kv.Key)
			break
		}
	}
	require.NotEmpty(t, selfKey)
	_, err = cli.Delete(context.Background(), selfKey)
	require.NoError(t, err)
	select {
	case campaignErr := <-otherCampaignDone:
		require.NoError(t, campaignErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out promoting the replacement leader")
	}

	releaseGet()
	select {
	case leader := <-leaders:
		require.Equal(t, "frontend-other", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the replacement before Campaign returned")
	}
	releaseCampaign()
	select {
	case <-blockingCampaign.secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("foreign observation did not queue a replacement proposal")
	}
	helpers.ShouldEventuallyReturn(t, func() bool {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return false
		}
		for _, kv := range response.Kvs {
			if string(kv.Value) == self.ID {
				return true
			}
		}
		return false
	}, true, 10*time.Millisecond, 5*time.Second)
	require.NoError(t, otherElection.Resign(context.Background()))
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the replacement proposal")
	}

	cancelGeneration()
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
		generationStopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping election generation")
	}
}

func TestEtcdElectionCampaignResultBeforeObservationRecampaigns(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	prefix := "election/frontend"
	leaderSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, leaderSession.Close()) })
	leaderElection := concurrency.NewElection(leaderSession, prefix)
	require.NoError(t, leaderElection.Campaign(context.Background(), "frontend-leader"))

	self := NewServer("frontend-self", "frontend", true, nil)
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	blockingKV := &blockElectionGetKV{
		KV:                    cli.KV,
		blockAt:               2,
		controlCtx:            generationCtx,
		started:               make(chan struct{}),
		release:               make(chan struct{}),
		firstCampaignCanceled: make(chan struct{}),
		secondTxnStarted:      make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseGet := func() { releaseOnce.Do(func() { close(blockingKV.release) }) }
	generationClient := newTestClientView(generationCtx, cli)
	generationClient.KV = blockingKV
	generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
		concurrency.WithContext(generationCtx))
	require.NoError(t, err)
	t.Cleanup(generationSession.Orphan)

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  generationSession,
		election: concurrency.NewElection(generationSession, sd.electionName),
	}
	leaders := make(chan string, 4)
	result := make(chan bool, 1)
	generationStopped := false
	t.Cleanup(func() {
		if generationStopped {
			return
		}
		cancelGeneration()
		releaseGet()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("timed out cleaning up election generation")
		}
	})
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case leader := <-leaders:
		require.Equal(t, "frontend-leader", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the initial leader")
	}
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)

	otherSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherSession.Close()) })
	otherElection := concurrency.NewElection(otherSession, prefix)
	otherCampaignDone := make(chan error, 1)
	go func() {
		otherCampaignDone <- otherElection.Campaign(context.Background(), "frontend-other")
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 3, 10*time.Millisecond, 5*time.Second)

	require.NoError(t, leaderElection.Resign(context.Background()))
	select {
	case <-blockingKV.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out blocking Observe after the initial leader resigned")
	}
	select {
	case <-blockingKV.firstCampaignCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not consume the successful Campaign result")
	}

	response, err := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	var selfKey string
	for _, kv := range response.Kvs {
		if string(kv.Value) == self.ID {
			selfKey = string(kv.Key)
			break
		}
	}
	require.NotEmpty(t, selfKey)
	_, err = cli.Delete(context.Background(), selfKey)
	require.NoError(t, err)
	select {
	case campaignErr := <-otherCampaignDone:
		require.NoError(t, campaignErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out promoting the replacement leader")
	}
	releaseGet()

	select {
	case leader := <-leaders:
		require.Equal(t, "frontend-other", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the replacement leader")
	}
	select {
	case <-blockingKV.secondTxnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("successful Campaign result did not trigger a replacement proposal")
	}
	helpers.ShouldEventuallyReturn(t, func() bool {
		response, getErr := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
		if getErr != nil {
			return false
		}
		for _, kv := range response.Kvs {
			if string(kv.Value) == self.ID {
				return true
			}
		}
		return false
	}, true, 10*time.Millisecond, 5*time.Second)
	require.NoError(t, otherElection.Resign(context.Background()))
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the replacement proposal")
	}

	cancelGeneration()
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
		generationStopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping election generation")
	}
}

func TestEtcdElectionLeaderReplacementQueuesRecampaign(t *testing.T) {
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	self := NewServer("frontend-self", "frontend", true, nil)
	generationCtx, cancelGeneration := context.WithCancel(context.Background())
	blockingKV := &postCommitBlockingPutKV{
		KV:             cli.KV,
		firstCommitted: make(chan struct{}),
		secondStarted:  make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(blockingKV.releaseFirst) }) }
	generationClient := newTestClientView(generationCtx, cli)
	generationClient.KV = blockingKV
	generationSession, err := concurrency.NewSession(generationClient, concurrency.WithTTL(3),
		concurrency.WithContext(generationCtx))
	require.NoError(t, err)
	t.Cleanup(generationSession.Orphan)

	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	sd := getEtcdSD(t, cfg, self, cli)
	generation := &electionGeneration{
		token:    1,
		ctx:      generationCtx,
		cancel:   cancelGeneration,
		session:  generationSession,
		election: concurrency.NewElection(generationSession, sd.electionName),
	}
	leaders := make(chan string, 3)
	result := make(chan bool, 1)
	generationStopped := false
	t.Cleanup(func() {
		if generationStopped {
			return
		}
		cancelGeneration()
		releaseFirst()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("timed out cleaning up election generation")
		}
	})
	go func() {
		_, reconnect := sd.runElectionGeneration(generationCtx, generation, func(leader string) bool {
			leaders <- leader
			return true
		})
		result <- reconnect
	}()

	select {
	case <-blockingKV.firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first campaign commit")
	}
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the first campaign")
	}

	otherSession, err := concurrency.NewSession(cli, concurrency.WithTTL(3))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherSession.Close()) })
	otherElection := concurrency.NewElection(otherSession, sd.electionName)
	otherCampaignDone := make(chan error, 1)
	go func() {
		otherCampaignDone <- otherElection.Campaign(context.Background(), "frontend-other")
	}()
	helpers.ShouldEventuallyReturn(t, func() int {
		response, getErr := cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
		if getErr != nil {
			return -1
		}
		return len(response.Kvs)
	}, 2, 10*time.Millisecond, 5*time.Second)

	response, err := cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	var selfKey string
	for _, kv := range response.Kvs {
		if string(kv.Value) == self.ID {
			selfKey = string(kv.Key)
			break
		}
	}
	require.NotEmpty(t, selfKey)
	_, err = cli.Delete(context.Background(), selfKey)
	require.NoError(t, err)
	select {
	case campaignErr := <-otherCampaignDone:
		require.NoError(t, campaignErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out promoting replacement leader")
	}
	select {
	case leader := <-leaders:
		require.Equal(t, "frontend-other", leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing replacement leader")
	}

	releaseFirst()
	select {
	case <-blockingKV.secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement leader did not queue a fresh campaign")
	}
	helpers.ShouldEventuallyReturn(t, func() bool {
		response, getErr := cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
		if getErr != nil {
			return false
		}
		for _, kv := range response.Kvs {
			if string(kv.Value) == self.ID {
				return true
			}
		}
		return false
	}, true, 10*time.Millisecond, 5*time.Second)
	require.NoError(t, otherElection.Resign(context.Background()))
	select {
	case leader := <-leaders:
		require.Equal(t, self.ID, leader)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out observing the queued recampaign")
	}

	cancelGeneration()
	select {
	case reconnect := <-result:
		assert.False(t, reconnect)
		generationStopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("timed out stopping election generation")
	}
}

func TestEtcdElectionLeaderCallbackCanShutdown(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.Election.Enable = true
	cfg.Heartbeat.TTL = 3 * time.Second
	cfg.Revoke.Timeout = time.Second
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, cli)
	sd.reconnectBackOff = 10 * time.Millisecond
	require.NoError(t, sd.Init())
	helpers.ShouldEventuallyReturn(t, sd.IsLeader, true, 10*time.Millisecond, 5*time.Second)

	shutdownDone := make(chan error, 1)
	sd.OnLeaderChange(func(leader string) {
		if leader == "" {
			shutdownDone <- sd.Shutdown()
		}
	})
	response, err := sd.cli.Get(context.Background(), sd.electionName+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	_, err = cli.Revoke(context.Background(), clientv3.LeaseID(response.Kvs[0].Lease))
	require.NoError(t, err)

	select {
	case err := <-shutdownDone:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for election leader callback shutdown")
	}
}

func TestEtcdWatchChangesAddNewServers(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = time.Duration(100 * time.Millisecond)
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			serversBefore, err := e.GetServersByType(table.server.Type)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(serversBefore))
			newServer := NewServer("newID", table.server.Type, false, map[string]string{})
			err = e.addServerIntoEtcd(newServer)
			assert.NoError(t, err)
			ss, err := getServerFromEtcd(e.cli, newServer.Type, newServer.ID)
			assert.NoError(t, err)
			assert.Equal(t, newServer, ss)
			helpers.ShouldEventuallyReturn(t, func() int {
				serversNow, _ := e.GetServersByType(table.server.Type)
				return len(serversNow)
			}, 2)
		})
	}
}

func TestEtcdWatchChangesDeleteServers(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = 100 * time.Millisecond
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())
			serversBefore, err := e.GetServersByType(table.server.Type)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(serversBefore))
			newServer := NewServer("newID", table.server.Type, false, map[string]string{})
			err = e.addServerIntoEtcd(newServer)
			assert.NoError(t, err)
			ss, err := getServerFromEtcd(e.cli, newServer.Type, newServer.ID)
			assert.NoError(t, err)
			assert.Equal(t, newServer, ss)
			helpers.ShouldEventuallyReturn(t, func() int {
				serversNow, _ := e.GetServersByType(table.server.Type)
				return len(serversNow)
			}, 2)
			_, err = e.cli.Delete(context.TODO(), getKey(newServer.ID, newServer.Type))
			assert.NoError(t, err)
			helpers.ShouldEventuallyReturn(t, func() int {
				serversNow, _ := e.GetServersByType(table.server.Type)
				return len(serversNow)
			}, 1)
		})
	}
}

func TestEtcdWatchChangesWithBlacklist(t *testing.T) {
	for _, table := range etcdSDBlacklistTables {
		t.Run(table.name, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = 100 * time.Millisecond
			config.ServerTypesBlacklist = table.serverTypeBlacklist
			c, cli := helpers.GetTestEtcd(t)
			t.Cleanup(func() { c.Terminate(t) })
			e := getEtcdSD(t, config, table.server, cli)
			require.NoError(t, e.Init())

			serversBefore, err := e.GetServersByType(table.server.Type)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(serversBefore))

			// Add all servers to ETCD
			for _, serverToAdd := range table.serversToAdd {
				_, err := e.cli.Put(
					context.TODO(),
					getKey(serverToAdd.ID, serverToAdd.Type),
					serverToAdd.AsJSONString(),
				)
				assert.NoError(t, err)
			}

			numRequiredServersInSd := func() int {
				num := 0
				for _, serverToAdd := range table.serversToAdd {
					if !e.isServerTypeBlacklisted(serverToAdd.Type) {
						num++
					}
				}
				return num + 1 // The server itself is always added to the cache, that's why we add one here
			}

			numServersInSd := func() int {
				num := 0

				_, err := e.GetServer(table.server.ID)
				if err == nil {
					num++
				}
				for _, serverToAdd := range table.serversToAdd {
					_, err := e.GetServer(serverToAdd.ID)
					if err == nil {
						num++
					}
				}
				return num
			}

			helpers.ShouldEventuallyReturn(t, numServersInSd, numRequiredServersInSd())
		})
	}
}

func TestEtcdSDBlacklistSelfExceptionRequiresMatchingTypeAndID(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	cfg.SyncServers.Interval = time.Hour
	cfg.ServerTypesBlacklist = []string{"room"}
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	self := NewServer("shared-id", "frontend", true, map[string]string{"owner": "self"})
	sd := getEtcdSD(t, cfg, self, cli)
	require.NoError(t, sd.Init())

	blacklisted := NewServer(self.ID, "room", false, map[string]string{"owner": "other-type"})
	_, err := sd.cli.Put(context.Background(), getKey(blacklisted.ID, blacklisted.Type), blacklisted.AsJSONString())
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(true))
	actual, err := sd.GetServer(self.ID)
	require.NoError(t, err)
	assert.Equal(t, self.Type, actual.Type)
	assert.Equal(t, "self", actual.Metadata["owner"])

	_, err = sd.cli.Delete(context.Background(), getKey(blacklisted.ID, blacklisted.Type))
	require.NoError(t, err)
	require.NoError(t, sd.SyncServers(true))
	actual, err = sd.GetServer(self.ID)
	require.NoError(t, err)
	assert.Equal(t, self.Type, actual.Type)
}

func TestParallelGetter(t *testing.T) {
	c, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { c.Terminate(t) })

	serversToAdd := []*Server{
		NewServer("frontend-1", "type1", true, nil),
		NewServer("frontend-2", "type2", true, nil),
		NewServer("frontend-3", "type3", true, nil),
		NewServer("frontend-4", "type4", true, nil),
		NewServer("frontend-5", "type5", true, nil),
		NewServer("frontend-6", "type6", true, nil),
		NewServer("frontend-7", "type7", true, nil),
		NewServer("frontend-8", "type8", true, nil),
		NewServer("frontend-9", "type9", true, nil),
	}

	// Add server
	for _, serverToAdd := range serversToAdd {
		_, err := cli.Put(
			context.TODO(),
			getKey(serverToAdd.ID, serverToAdd.Type),
			serverToAdd.AsJSONString(),
		)
		assert.NoError(t, err)
	}

	parallelGetter, err := newParallelGetter(context.Background(), cli, 5)
	require.NoError(t, err)
	for _, serverToAdd := range serversToAdd {
		payload := []byte("{\"id\":\"" + serverToAdd.ID + "\",\"type\":\"" + serverToAdd.Type + "\",\"frontend\":true,\"hostname\":\"" + serverToAdd.ID + "\",\"metadata\":{\"region\":\"us\"}}")
		parallelGetter.addWorkWithPayload(serverToAdd.Type, serverToAdd.ID, payload, 1)
	}

	servers, err := parallelGetter.waitAndGetResult()
	require.NoError(t, err)
	assert.Equal(t, len(serversToAdd), len(servers))

	// We add the returned servers to a map first, since the results may be out the order they were added.
	serversMap := map[string]*Server{}
	for _, versioned := range servers {
		sv := versioned.server
		_, ok := serversMap[sv.ID]
		assert.False(t, ok)
		serversMap[sv.ID] = sv
	}

	for _, serverToAdd := range serversToAdd {
		sv, ok := serversMap[serverToAdd.ID]
		assert.True(t, ok)
		assert.Equal(t, serverToAdd.Type, sv.Type)
		assert.Equal(t, serverToAdd.ID, sv.ID)
		assert.Equal(t, serverToAdd.Frontend, sv.Frontend)
	}
}

func TestParallelGetterConsumesMalformedPayloadsWithoutHanging(t *testing.T) {
	getter, err := newParallelGetter(context.Background(), nil, 1)
	require.NoError(t, err)
	payloads := []string{
		"",
		"{",
		"null",
		"{}",
		NewServer("other", "room", false, nil).AsJSONString(),
		NewServer("room-5", "other", false, nil).AsJSONString(),
	}
	for i, payload := range payloads {
		getter.addWorkWithPayload("room", fmt.Sprintf("room-%d", i), []byte(payload), int64(i+1))
	}

	type getterResult struct {
		servers []versionedServer
		err     error
	}
	result := make(chan getterResult, 1)
	go func() {
		servers, waitErr := getter.waitAndGetResult()
		result <- getterResult{servers: servers, err: waitErr}
	}()
	select {
	case actual := <-result:
		require.NoError(t, actual.err)
		require.Len(t, actual.servers, len(payloads))
		for _, versioned := range actual.servers {
			assert.Nil(t, versioned.server)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parallel getter did not complete malformed work")
	}
}

func TestParallelGetterCompletesPanickingWork(t *testing.T) {
	getter, err := newParallelGetter(context.Background(), nil, 1)
	require.NoError(t, err)
	getter.addWork("room", "panic", 1)
	valid := NewServer("room-valid", "room", false, nil)
	getter.addWorkWithPayload(valid.Type, valid.ID, []byte(valid.AsJSONString()), 2)

	type getterResult struct {
		servers []versionedServer
		err     error
	}
	result := make(chan getterResult, 1)
	go func() {
		servers, waitErr := getter.waitAndGetResult()
		result <- getterResult{servers: servers, err: waitErr}
	}()
	select {
	case actual := <-result:
		require.ErrorContains(t, actual.err, "panic loading server room/panic from etcd")
		require.Len(t, actual.servers, 1)
		assert.Equal(t, valid.ID, actual.servers[0].server.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("parallel getter did not complete after a panicking work item")
	}
}

func TestNewParallelGetterPropagatesSubmissionError(t *testing.T) {
	if os.Getenv(statelessSubmissionErrorHelperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNewParallelGetterPropagatesSubmissionError$")
		cmd.Env = append(os.Environ(), statelessSubmissionErrorHelperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("parallel getter submission helper failed: %v\n%s", err, output)
		}
		return
	}

	ants.Release()
	getter, err := newParallelGetter(context.Background(), nil, 5)
	require.ErrorIs(t, err, ants.ErrPoolClosed)
	assert.Nil(t, getter)
}
