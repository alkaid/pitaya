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

package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/alkaid/pitaya/v2/route"
	protobufserializer "github.com/alkaid/pitaya/v2/serialize/protobuf"
	"github.com/alkaid/pitaya/v2/session"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

type statefulOwnerTestDiscovery struct {
	cluster.ServiceDiscovery
	self      *cluster.Server
	resolve   func(context.Context, string, string, string) (*cluster.Server, cluster.StatefulOwnerFence, error)
	validate  func(context.Context, string, string, cluster.StatefulOwnerFence) error
	getServer func(string) (*cluster.Server, error)
}

func (d *statefulOwnerTestDiscovery) GetSelfServer() *cluster.Server {
	return d.self
}

func (d *statefulOwnerTestDiscovery) GetServer(serverID string) (*cluster.Server, error) {
	if d.getServer == nil {
		return nil, errors.New("unexpected server lookup")
	}
	return d.getServer(serverID)
}

func (d *statefulOwnerTestDiscovery) ResolveStatefulOwner(
	ctx context.Context,
	serverType string,
	routingKey string,
	candidateServerID string,
) (*cluster.Server, cluster.StatefulOwnerFence, error) {
	if d.resolve == nil {
		return nil, cluster.StatefulOwnerFence{}, errors.New("unexpected stateful owner resolution")
	}
	return d.resolve(ctx, serverType, routingKey, candidateServerID)
}

func (d *statefulOwnerTestDiscovery) ValidateStatefulOwner(
	ctx context.Context,
	serverType string,
	routingKey string,
	fence cluster.StatefulOwnerFence,
) error {
	if d.validate == nil {
		return errors.New("unexpected stateful owner validation")
	}
	return d.validate(ctx, serverType, routingKey, fence)
}

type statefulOwnerTestRPCClient struct {
	cluster.RPCClient
	call func(
		context.Context,
		protos.RPCType,
		*route.Route,
		session.Session,
		*message.Message,
		*cluster.Server,
	) (*protos.Response, error)
}

func (c *statefulOwnerTestRPCClient) Call(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	sess session.Session,
	msg *message.Message,
	server *cluster.Server,
) (*protos.Response, error) {
	return c.call(ctx, rpcType, route, sess, msg, server)
}

type RPCStatefulOwnerInfoRemote struct {
	component.Base
	observed       chan RPCStatefulOwnerInfo
	rawMetadata    chan bool
	nextHopHasInfo chan bool
}

func (r *RPCStatefulOwnerInfoRemote) Inspect(
	ctx context.Context,
	input *test.SomeStruct,
) (*test.SomeStruct, error) {
	info, ok := RPCStatefulOwnerInfoFromContext(ctx)
	if !ok {
		return nil, errors.New("RPC stateful owner info missing from current handler")
	}
	r.observed <- info
	propagated := pcontext.ToMap(ctx)
	_, hasKey := propagated[constants.StatefulOwnerKey]
	_, hasEpoch := propagated[constants.StatefulOwnerEpochKey]
	_, hasIdempotency := propagated[constants.IdempotencyKey]
	_, hasFanout := propagated[constants.IdempotencyFanoutKey]
	_, hasRouteMode := propagated[rpccontext.StatefulRouteModeKey]
	_, hasTypedRouteMode := rpccontext.StatefulRouteModeFromContext(ctx)
	r.rawMetadata <- hasKey || hasEpoch || hasIdempotency || hasFanout || hasRouteMode ||
		hasTypedRouteMode || rpccontext.RPCFanoutFromContext(ctx)

	metadata, err := pcontext.Encode(ctx)
	if err != nil {
		return nil, err
	}
	nextHop, err := pcontext.Decode(metadata)
	if err != nil {
		return nil, err
	}
	_, nextHopHasInfo := RPCStatefulOwnerInfoFromContext(nextHop)
	r.nextHopHasInfo <- nextHopHasInfo
	return input, nil
}

func statefulOwnerAdmissionContext(key string, epoch uint64) context.Context {
	return pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.StatefulOwnerKey, key,
		constants.StatefulOwnerEpochKey, strconv.FormatUint(epoch, 10),
		rpccontext.StatefulRouteModeKey, string(rpccontext.StatefulRouteModeAssigned),
		"tenant", "east",
	)
}

func TestRemoteCallKeepsExplicitServerIDDirectAndClearsSpoofedAuthority(t *testing.T) {
	target := cluster.NewServer("game-1", "game", false)
	discovery := &statefulOwnerTestDiscovery{
		getServer: func(serverID string) (*cluster.Server, error) {
			assert.Equal(t, target.ID, serverID)
			return target, nil
		},
		resolve: func(
			context.Context,
			string,
			string,
			string,
		) (*cluster.Server, cluster.StatefulOwnerFence, error) {
			t.Fatal("an explicit server ID must not be replaced by an assignment")
			return nil, cluster.StatefulOwnerFence{}, nil
		},
	}
	client := &statefulOwnerTestRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		assert.Same(t, target, server)
		assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.StatefulOwnerKey))
		assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.StatefulOwnerEpochKey))
		assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey))
		assert.Equal(
			t,
			string(rpccontext.StatefulRouteModeDirect),
			pcontext.GetFromPropagateCtx(ctx, rpccontext.StatefulRouteModeKey),
		)
		mode, ok := rpccontext.StatefulRouteModeFromContext(ctx)
		require.True(t, ok)
		assert.Equal(t, rpccontext.StatefulRouteModeDirect, mode)
		assert.False(t, rpccontext.RPCFanoutFromContext(ctx))
		return &protos.Response{}, nil
	}}
	remote := NewRemoteService(
		client, nil, discovery, nil, nil, nil, nil,
		cluster.NewServer("caller-1", "caller", false), nil, nil, nil, nil,
	)
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	spoofed := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.StatefulOwnerKey, strings.Repeat("a", 64),
		constants.StatefulOwnerEpochKey, "73",
		constants.IdempotencyFanoutKey, "1",
		rpccontext.StatefulRouteModeKey, string(rpccontext.StatefulRouteModeFanout),
	)
	_, err := remote.DoRPC(
		spoofed,
		target.ID,
		route.NewRoute("game", "player", "move"),
		nil,
		sess,
	)
	require.NoError(t, err)
	_, originalWasMutated := target.StatefulOwnerFenceSnapshot()
	assert.False(t, originalWasMutated)
}

func TestRemoteCallRawFanoutSpoofCannotBypassAssignedTarget(t *testing.T) {
	target := cluster.NewServer("game-1", "game", false)
	wantFence := cluster.StatefulOwnerFence{
		AssignmentKey: strings.Repeat("a", 64),
		Epoch:         73,
		ServerID:      target.ID,
	}
	assignedTarget := target.WithStatefulOwnerFence(wantFence)
	client := &statefulOwnerTestRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		assert.Same(t, assignedTarget, server)
		assert.Equal(t, wantFence.AssignmentKey, pcontext.GetFromPropagateCtx(ctx, constants.StatefulOwnerKey))
		assert.Equal(t, "73", pcontext.GetFromPropagateCtx(ctx, constants.StatefulOwnerEpochKey))
		assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey))
		assert.Equal(
			t,
			string(rpccontext.StatefulRouteModeAssigned),
			pcontext.GetFromPropagateCtx(ctx, rpccontext.StatefulRouteModeKey),
		)
		mode, ok := rpccontext.StatefulRouteModeFromContext(ctx)
		require.True(t, ok)
		assert.Equal(t, rpccontext.StatefulRouteModeAssigned, mode)
		assert.False(t, rpccontext.RPCFanoutFromContext(ctx))
		return &protos.Response{}, nil
	}}
	remote := NewRemoteService(
		client, nil, nil, nil, nil, nil, nil,
		cluster.NewServer("caller-1", "caller", false), nil, nil, nil, nil,
	)
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	spoofed := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.StatefulOwnerKey, strings.Repeat("b", 64),
		constants.StatefulOwnerEpochKey, "99",
		constants.IdempotencyFanoutKey, "1",
		rpccontext.StatefulRouteModeKey, string(rpccontext.StatefulRouteModeFanout),
	)

	_, err := remote.remoteCall(
		spoofed,
		assignedTarget,
		protos.RPCType_User,
		route.NewRoute("game", "player", "move"),
		sess,
		&message.Message{Type: message.Request},
	)
	require.NoError(t, err)
}

func TestRemoteCallDoesNotCollapseFanoutOntoOneStatefulOwner(t *testing.T) {
	target := cluster.NewServer("game-1", "game", false)
	discovery := &statefulOwnerTestDiscovery{resolve: func(
		context.Context,
		string,
		string,
		string,
	) (*cluster.Server, cluster.StatefulOwnerFence, error) {
		t.Fatal("fanout must retain its explicit recipient")
		return nil, cluster.StatefulOwnerFence{}, nil
	}}
	client := &statefulOwnerTestRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		assert.Same(t, target, server)
		assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.StatefulOwnerKey))
		assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.StatefulOwnerEpochKey))
		assert.Equal(t, "1", pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey))
		assert.Equal(
			t,
			string(rpccontext.StatefulRouteModeFanout),
			pcontext.GetFromPropagateCtx(ctx, rpccontext.StatefulRouteModeKey),
		)
		assert.True(t, rpccontext.RPCFanoutFromContext(ctx))
		return &protos.Response{}, nil
	}}
	remote := NewRemoteService(
		client, nil, discovery, nil, nil, nil, nil,
		cluster.NewServer("caller-1", "caller", false), nil, nil, nil, nil,
	)
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	ctx := withFanoutIdempotencyScope(context.Background())
	_, err := remote.remoteCall(
		ctx,
		target,
		protos.RPCType_User,
		route.NewRoute("game", "player", "move"),
		sess,
		&message.Message{Type: message.Request},
	)
	require.NoError(t, err)
}

func TestRPCStatefulOwnerInfoIsTypedAndAdmissionMetadataIsConsumed(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background()} {
		info, ok := RPCStatefulOwnerInfoFromContext(ctx)
		assert.False(t, ok)
		assert.Empty(t, info)
	}

	server := cluster.NewServer("game-1", "game", false)
	routingIdentity, err := cluster.StatefulOwnerRoutingKey("user-1", "", 0)
	require.NoError(t, err)
	wantFence := cluster.StatefulOwnerFence{
		AssignmentKey: strings.Repeat("a", 64),
		Epoch:         73,
		ServerID:      server.ID,
	}
	validateCalls := 0
	discovery := &statefulOwnerTestDiscovery{self: server, validate: func(
		_ context.Context,
		serverType string,
		routingKey string,
		fence cluster.StatefulOwnerFence,
	) error {
		validateCalls++
		assert.Equal(t, server.Type, serverType)
		assert.Equal(t, routingIdentity, routingKey)
		assert.Equal(t, wantFence, fence)
		return nil
	}}
	store := &recordingIdempotencyStore{invoke: true}
	remote := newRemoteCallService(server, WithIdempotencyStore(store))
	remote.serviceDiscovery = discovery
	handler := &RPCStatefulOwnerInfoRemote{
		observed:       make(chan RPCStatefulOwnerInfo, 1),
		rawMetadata:    make(chan bool, 1),
		nextHopHasInfo: make(chan bool, 1),
	}
	require.NoError(t, remote.Register(handler, nil))

	data, err := proto.Marshal(&test.SomeStruct{B: "accepted"})
	require.NoError(t, err)
	request := &protos.Request{
		Type:       protos.RPCType_User,
		FrontendID: "frontend-1",
		Session:    &protos.Session{Id: 1, Uid: "user-1"},
		Msg: &protos.Msg{
			Route: "RPCStatefulOwnerInfoRemote.Inspect",
			Data:  data,
		},
	}
	ctx := pcontext.AddListToPropagateCtx(
		statefulOwnerAdmissionContext(wantFence.AssignmentKey, wantFence.Epoch),
		constants.IdempotencyKey, "command-1",
	)
	request = withRequestMetadata(t, request, ctx)
	response, err := remote.Call(ctx, request)
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Nil(t, response.Status)
	assert.Equal(t, 4, validateCalls, "the assignment must be checked before pre-handler callbacks and component invocation")

	wantInfo := RPCStatefulOwnerInfo{
		AssignmentKey: wantFence.AssignmentKey,
		Epoch:         wantFence.Epoch,
		ServerID:      server.ID,
		ServerType:    server.Type,
	}
	assert.Equal(t, wantInfo, <-handler.observed)
	assert.False(t, <-handler.rawMetadata)
	assert.False(t, <-handler.nextHopHasInfo)
	storeInfo, ok := RPCStatefulOwnerInfoFromContext(store.ctx)
	require.True(t, ok)
	assert.Equal(t, wantInfo, storeInfo)
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.StatefulOwnerKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.StatefulOwnerEpochKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.IdempotencyKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.IdempotencyFanoutKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, rpccontext.StatefulRouteModeKey))
	_, hasStoreRouteMode := rpccontext.StatefulRouteModeFromContext(store.ctx)
	assert.False(t, hasStoreRouteMode)
	assert.False(t, rpccontext.RPCFanoutFromContext(store.ctx))
}

func TestStatefulOwnerAdmissionFailsClosed(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	discovery := &statefulOwnerTestDiscovery{validate: func(
		context.Context,
		string,
		string,
		cluster.StatefulOwnerFence,
	) error {
		return constants.ErrStaleStatefulOwner
	}}
	remote := newRemoteCallService(server)
	remote.serviceDiscovery = discovery
	request := &protos.Request{Session: &protos.Session{Id: 1, Uid: "user-1"}}

	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing", ctx: context.Background()},
		{
			name: "missing epoch",
			ctx: pcontext.AddToPropagateCtx(
				context.Background(),
				constants.StatefulOwnerKey,
				strings.Repeat("a", 64),
			),
		},
		{
			name: "wrong epoch type",
			ctx: pcontext.AddListToPropagateCtx(
				context.Background(),
				constants.StatefulOwnerKey, strings.Repeat("a", 64),
				constants.StatefulOwnerEpochKey, uint64(73),
			),
		},
		{
			name: "zero epoch",
			ctx: pcontext.AddListToPropagateCtx(
				context.Background(),
				constants.StatefulOwnerKey, strings.Repeat("a", 64),
				constants.StatefulOwnerEpochKey, "0",
			),
		},
		{
			name: "stale",
			ctx:  statefulOwnerAdmissionContext(strings.Repeat("a", 64), 73),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := remote.admitStatefulOwner(test.ctx, request)
			assert.ErrorIs(t, err, constants.ErrStaleStatefulOwner)
		})
	}

	fanout := withFanoutIdempotencyScope(context.Background())
	admission, err := remote.admitStatefulOwner(fanout, request)
	require.NoError(t, err)
	assert.False(t, admission.admitted())
}

func TestStatefulOwnerMetadataRequiresValidator(t *testing.T) {
	remote := newRemoteCallService(cluster.NewServer("game-1", "game", false))
	ctx := statefulOwnerAdmissionContext(strings.Repeat("a", 64), 73)
	_, err := remote.admitStatefulOwner(ctx, &protos.Request{})
	assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentUnavailable)
}

func TestStatefulOwnerAdmissionBindsFenceToRequestRoutingKey(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	routingIdentity, err := cluster.StatefulOwnerRoutingKey("user-1", "", 0)
	require.NoError(t, err)
	wantFence := cluster.StatefulOwnerFence{
		AssignmentKey: strings.Repeat("a", 64),
		Epoch:         73,
		ServerID:      server.ID,
	}
	var validateCalls int
	discovery := &statefulOwnerTestDiscovery{validate: func(
		_ context.Context,
		serverType string,
		routingKey string,
		fence cluster.StatefulOwnerFence,
	) error {
		validateCalls++
		assert.Equal(t, server.Type, serverType)
		assert.Equal(t, wantFence, fence)
		if routingKey != routingIdentity {
			return constants.ErrStaleStatefulOwner
		}
		return nil
	}}
	remote := newRemoteCallService(server)
	remote.serviceDiscovery = discovery

	_, err = remote.admitStatefulOwner(
		statefulOwnerAdmissionContext(wantFence.AssignmentKey, wantFence.Epoch),
		&protos.Request{Session: &protos.Session{Id: 1, Uid: "user-2"}},
	)
	assert.ErrorIs(t, err, constants.ErrStaleStatefulOwner)
	assert.Equal(t, 1, validateCalls)
}

func TestStatefulOwnerAdmissionSeparatesUIDAndFrontendSessionDomains(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	uidIdentity, err := cluster.StatefulOwnerRoutingKey("frontend-1", "", 0)
	require.NoError(t, err)
	sessionIdentity, err := cluster.StatefulOwnerRoutingKey("", "frontend", 1)
	require.NoError(t, err)
	require.NotEqual(t, uidIdentity, sessionIdentity)
	uidKey, err := cluster.StatefulOwnerAssignmentKey(server.Type, uidIdentity)
	require.NoError(t, err)
	sessionKey, err := cluster.StatefulOwnerAssignmentKey(server.Type, sessionIdentity)
	require.NoError(t, err)
	require.NotEqual(t, uidKey, sessionKey)

	discovery := &statefulOwnerTestDiscovery{validate: func(
		_ context.Context,
		serverType string,
		routingIdentity string,
		fence cluster.StatefulOwnerFence,
	) error {
		expected, keyErr := cluster.StatefulOwnerAssignmentKey(serverType, routingIdentity)
		if keyErr != nil {
			return keyErr
		}
		if fence.AssignmentKey != expected {
			return constants.ErrStaleStatefulOwner
		}
		return nil
	}}
	remote := newRemoteCallService(server)
	remote.serviceDiscovery = discovery

	for _, replay := range []struct {
		name string
		key  string
		req  *protos.Request
	}{
		{
			name: "uid token onto frontend session",
			key:  uidKey,
			req:  &protos.Request{FrontendID: "frontend", Session: &protos.Session{Id: 1}},
		},
		{
			name: "frontend session token onto uid",
			key:  sessionKey,
			req:  &protos.Request{Session: &protos.Session{Uid: "frontend-1"}},
		},
	} {
		t.Run(replay.name, func(t *testing.T) {
			_, err := remote.admitStatefulOwner(statefulOwnerAdmissionContext(replay.key, 73), replay.req)
			assert.ErrorIs(t, err, constants.ErrStaleStatefulOwner)
		})
	}
}

type blockingIdempotencyStore struct {
	waiting chan struct{}
	release chan struct{}
	ctx     context.Context
}

type blockingAdmissionTaskExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (e *blockingAdmissionTaskExecutor) Execute(ctx context.Context, task component.Task) error {
	close(e.started)
	<-e.release
	return task(ctx)
}

func (s *blockingIdempotencyStore) Execute(
	ctx context.Context,
	_ string,
	_ string,
	fn func(context.Context) (*protos.Response, error),
) (*protos.Response, error) {
	s.ctx = ctx
	close(s.waiting)
	<-s.release
	return fn(ctx)
}

func TestStatefulOwnerIsRevalidatedAfterIdempotencyWait(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	routingIdentity, err := cluster.StatefulOwnerRoutingKey("user-1", "", 0)
	require.NoError(t, err)
	wantFence := cluster.StatefulOwnerFence{
		AssignmentKey: strings.Repeat("a", 64),
		Epoch:         73,
		ServerID:      server.ID,
	}
	var stale atomic.Bool
	var validateCalls atomic.Int32
	discovery := &statefulOwnerTestDiscovery{validate: func(
		_ context.Context,
		serverType string,
		routingKey string,
		fence cluster.StatefulOwnerFence,
	) error {
		validateCalls.Add(1)
		assert.Equal(t, server.Type, serverType)
		assert.Equal(t, routingIdentity, routingKey)
		assert.Equal(t, wantFence, fence)
		if stale.Load() {
			return constants.ErrStaleStatefulOwner
		}
		return nil
	}}
	store := &blockingIdempotencyStore{waiting: make(chan struct{}), release: make(chan struct{})}
	remote := newRemoteCallService(server, WithIdempotencyStore(store))
	remote.serviceDiscovery = discovery
	var handlerCalls atomic.Int32
	require.NoError(t, remote.Register(&CallCoverageRemote{calls: &handlerCalls}, nil))
	request := callCoverageRequest(t, "north")
	request.Session = &protos.Session{Id: 1, Uid: "user-1"}
	request.FrontendID = "frontend-1"
	ctx := pcontext.AddToPropagateCtx(
		statefulOwnerAdmissionContext(wantFence.AssignmentKey, wantFence.Epoch),
		constants.IdempotencyKey,
		"command-1",
	)
	request = withRequestMetadata(t, request, ctx)

	type outcome struct {
		response *protos.Response
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := remote.Call(ctx, request)
		done <- outcome{response: response, err: err}
	}()
	<-store.waiting
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.StatefulOwnerKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.StatefulOwnerEpochKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, constants.IdempotencyKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(store.ctx, rpccontext.StatefulRouteModeKey))
	_, hasRouteMode := rpccontext.StatefulRouteModeFromContext(store.ctx)
	assert.False(t, hasRouteMode)
	assert.False(t, rpccontext.RPCFanoutFromContext(store.ctx))
	_, hasOwnerInfo := RPCStatefulOwnerInfoFromContext(store.ctx)
	assert.True(t, hasOwnerInfo)

	stale.Store(true)
	close(store.release)
	result := <-done
	assert.ErrorIs(t, result.err, constants.ErrStaleStatefulOwner)
	require.NotNil(t, result.response)
	require.NotNil(t, result.response.Status)
	assert.Equal(t, int32(2), validateCalls.Load())
	assert.Zero(t, handlerCalls.Load(), "a stale callback must be rejected before the handler")
}

func TestOwnerRegistrationIsRevalidatedAfterIdempotencyWait(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	server.SetOwnerEpoch(73)
	store := &blockingIdempotencyStore{waiting: make(chan struct{}), release: make(chan struct{})}
	remote := newRemoteCallService(server, WithIdempotencyStore(store))
	var handlerCalls atomic.Int32
	require.NoError(t, remote.Register(&CallCoverageRemote{calls: &handlerCalls}, nil))

	request := callCoverageRequest(t, "north")
	request.Session = &protos.Session{Id: 1, Uid: "user-1"}
	request.FrontendID = "frontend-1"
	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, server.ID,
		constants.OwnerEpochKey, "73",
		constants.IdempotencyKey, "command-1",
		rpccontext.StatefulRouteModeKey, string(rpccontext.StatefulRouteModeDirect),
	)
	request = withRequestMetadata(t, request, ctx)

	type outcome struct {
		response *protos.Response
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := remote.Call(ctx, request)
		done <- outcome{response: response, err: err}
	}()
	<-store.waiting
	server.InvalidateOwnerEpoch()
	close(store.release)

	result := <-done
	assert.ErrorIs(t, result.err, constants.ErrStaleOwnerEpoch)
	require.NotNil(t, result.response)
	require.NotNil(t, result.response.Status)
	assert.Zero(t, handlerCalls.Load(), "an invalidated registration must be rejected before the handler")
}

func TestStatefulOwnerIsRevalidatedInsideRemoteTaskExecutor(t *testing.T) {
	customResolverErr := errors.New("custom resolver unavailable")
	for _, testCase := range []struct {
		name               string
		keyed              bool
		replaceHookContext bool
		rejection          error
	}{
		{name: "unkeyed handoff", rejection: constants.ErrStaleStatefulOwner},
		{name: "unkeyed handoff after hook context replacement", replaceHookContext: true, rejection: constants.ErrStaleStatefulOwner},
		{name: "keyed handoff", keyed: true, rejection: constants.ErrStaleStatefulOwner},
		{name: "keyed custom resolver error", keyed: true, rejection: customResolverErr},
		{name: "keyed resolver deadline", keyed: true, rejection: context.DeadlineExceeded},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := cluster.NewServer("game-1", "game", false)
			routingIdentity, err := cluster.StatefulOwnerRoutingKey("user-1", "", 0)
			require.NoError(t, err)
			assignmentKey, err := cluster.StatefulOwnerAssignmentKey(server.Type, routingIdentity)
			require.NoError(t, err)
			fence := cluster.StatefulOwnerFence{AssignmentKey: assignmentKey, Epoch: 73, ServerID: server.ID}
			var stale atomic.Bool
			var validateCalls atomic.Int32
			discovery := &statefulOwnerTestDiscovery{self: server, validate: func(
				_ context.Context,
				serverType string,
				gotIdentity string,
				gotFence cluster.StatefulOwnerFence,
			) error {
				validateCalls.Add(1)
				assert.Equal(t, server.Type, serverType)
				assert.Equal(t, routingIdentity, gotIdentity)
				assert.Equal(t, fence, gotFence)
				if stale.Load() {
					return testCase.rejection
				}
				return nil
			}}
			executor := &blockingAdmissionTaskExecutor{started: make(chan struct{}), release: make(chan struct{})}
			store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
			remote := newRemoteCallService(server, WithIdempotencyStore(store))
			remote.serviceDiscovery = discovery
			if testCase.replaceHookContext {
				remote.remoteHooks.BeforeHandler.PushBack(func(
					_ context.Context,
					_ route.Route,
					arg interface{},
				) (context.Context, interface{}, error) {
					return context.Background(), arg, nil
				})
			}
			var handlerCalls atomic.Int32
			require.NoError(t, remote.Register(&CallCoverageRemote{calls: &handlerCalls}, []component.Option{
				component.WithTaskExecutor(executor),
			}))

			request := callCoverageRequest(t, "north")
			request.Session = &protos.Session{Id: 1, Uid: "user-1"}
			request.FrontendID = "frontend-1"
			ctx := statefulOwnerAdmissionContext(fence.AssignmentKey, fence.Epoch)
			if testCase.keyed {
				ctx = pcontext.AddToPropagateCtx(ctx, constants.IdempotencyKey, "command-1")
			}
			request = withRequestMetadata(t, request, ctx)
			type outcome struct {
				response *protos.Response
				err      error
			}
			done := make(chan outcome, 1)
			go func() {
				response, callErr := remote.Call(ctx, request)
				done <- outcome{response: response, err: callErr}
			}()

			<-executor.started
			stale.Store(true)
			close(executor.release)
			result := <-done
			assert.ErrorIs(t, result.err, testCase.rejection)
			assert.True(t, IsRPCAdmissionRejected(result.err))
			require.NotNil(t, result.response)
			require.NotNil(t, result.response.Status)
			assert.Zero(t, handlerCalls.Load(), "the handler must not start after assignment handoff")
			wantValidations := int32(3)
			if testCase.keyed {
				wantValidations = 4
			}
			assert.Equal(t, wantValidations, validateCalls.Load())
			store.mu.Lock()
			assert.Empty(t, store.entries, "a final admission rejection must release the idempotency claim")
			store.mu.Unlock()
		})
	}
}

func TestOwnerRegistrationIsRevalidatedInsideSysHandlerTaskExecutor(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	server.SetOwnerEpoch(73)
	discovery := &statefulOwnerTestDiscovery{self: server}
	executor := &blockingAdmissionTaskExecutor{started: make(chan struct{}), release: make(chan struct{})}
	var handlerCalls atomic.Int32
	componentValue := &TaskDispatchComponent{calls: &handlerCalls}
	handlerService := component.NewService(componentValue, []component.Option{
		component.WithName("taskexecutor"),
		component.WithTaskExecutor(executor),
	})
	require.NoError(t, handlerService.ExtractHandler())
	handler, ok := handlerService.Handlers["Invoke"]
	require.True(t, ok)

	remote := newRemoteCallService(server)
	remote.serviceDiscovery = discovery
	remote.serializer = protobufserializer.NewSerializer()
	remote.handlerPool.Register("taskexecutor", "Invoke", handler)
	remote.handlerHooks.BeforeHandler.PushBack(func(
		_ context.Context,
		_ route.Route,
		arg interface{},
	) (context.Context, interface{}, error) {
		return context.Background(), arg, nil
	})
	data, err := proto.Marshal(&test.SomeStruct{B: "queued"})
	require.NoError(t, err)
	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, server.ID,
		constants.OwnerEpochKey, "73",
		rpccontext.StatefulRouteModeKey, string(rpccontext.StatefulRouteModeDirect),
	)
	request := withRequestMetadata(t, &protos.Request{
		Type:       protos.RPCType_Sys,
		FrontendID: "frontend-1",
		Session:    &protos.Session{Id: 1, Uid: "user-1"},
		Msg: &protos.Msg{
			Route: "taskexecutor.Invoke",
			Type:  protos.MsgType_MsgRequest,
			Data:  data,
		},
	}, ctx)
	type outcome struct {
		response *protos.Response
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, callErr := remote.Call(ctx, request)
		done <- outcome{response: response, err: callErr}
	}()

	<-executor.started
	server.InvalidateOwnerEpoch()
	close(executor.release)
	result := <-done
	assert.ErrorIs(t, result.err, constants.ErrStaleOwnerEpoch)
	assert.True(t, IsRPCAdmissionRejected(result.err))
	require.NotNil(t, result.response)
	require.NotNil(t, result.response.Status)
	assert.Zero(t, handlerCalls.Load(), "the sys handler must not start after registration invalidation")
}

func TestStatefulOwnerIsRevalidatedBeforeRemotePreHandlerCallbacks(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		keyed            bool
		receiverProvider bool
		rejectAt         int32
	}{
		{name: "unkeyed receiver provider", receiverProvider: true, rejectAt: 2},
		{name: "keyed before hook", keyed: true, rejectAt: 3},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := cluster.NewServer("game-1", "game", false)
			routingIdentity, err := cluster.StatefulOwnerRoutingKey("user-1", "", 0)
			require.NoError(t, err)
			assignmentKey, err := cluster.StatefulOwnerAssignmentKey(server.Type, routingIdentity)
			require.NoError(t, err)
			fence := cluster.StatefulOwnerFence{AssignmentKey: assignmentKey, Epoch: 73, ServerID: server.ID}
			var validateCalls atomic.Int32
			discovery := &statefulOwnerTestDiscovery{self: server, validate: func(
				_ context.Context,
				_ string,
				_ string,
				_ cluster.StatefulOwnerFence,
			) error {
				if validateCalls.Add(1) == testCase.rejectAt {
					return constants.ErrStaleStatefulOwner
				}
				return nil
			}}
			store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
			remote := newRemoteCallService(server, WithIdempotencyStore(store))
			remote.serviceDiscovery = discovery

			var callbackCalls atomic.Int32
			var handlerCalls atomic.Int32
			var options []component.Option
			if testCase.receiverProvider {
				options = append(options, component.WithReceiverProvider(func(context.Context) component.Component {
					callbackCalls.Add(1)
					return &CallCoverageRemote{calls: &handlerCalls}
				}))
			} else {
				remote.remoteHooks.BeforeHandler.PushBack(func(
					ctx context.Context,
					_ route.Route,
					arg interface{},
				) (context.Context, interface{}, error) {
					callbackCalls.Add(1)
					return ctx, arg, nil
				})
			}
			require.NoError(t, remote.Register(&CallCoverageRemote{calls: &handlerCalls}, options))

			request := callCoverageRequest(t, "north")
			request.Session = &protos.Session{Id: 1, Uid: "user-1"}
			request.FrontendID = "frontend-1"
			ctx := statefulOwnerAdmissionContext(fence.AssignmentKey, fence.Epoch)
			if testCase.keyed {
				ctx = pcontext.AddToPropagateCtx(ctx, constants.IdempotencyKey, "command-1")
			}
			request = withRequestMetadata(t, request, ctx)

			response, err := remote.Call(ctx, request)
			assert.ErrorIs(t, err, constants.ErrStaleStatefulOwner)
			assert.True(t, IsRPCAdmissionRejected(err))
			require.NotNil(t, response)
			require.NotNil(t, response.Status)
			assert.Zero(t, callbackCalls.Load(), "pre-handler callbacks must not start under a stale assignment")
			assert.Zero(t, handlerCalls.Load())
			assert.Equal(t, testCase.rejectAt, validateCalls.Load())
			store.mu.Lock()
			assert.Empty(t, store.entries, "a pre-handler admission rejection must release the idempotency claim")
			store.mu.Unlock()
		})
	}
}

func TestRPCAdmissionIsRevalidatedBeforeHandlerPoolCallbacks(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		receiverProvider bool
		rejectAt         int32
		wantHookCalls    int32
	}{
		{name: "before hook", rejectAt: 1},
		{name: "receiver provider", receiverProvider: true, rejectAt: 2, wantHookCalls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var validateCalls atomic.Int32
			ctx := withRPCAdmissionGuard(context.Background(), newRPCAdmissionGuard(func(context.Context) error {
				if validateCalls.Add(1) == testCase.rejectAt {
					return constants.ErrStaleOwnerEpoch
				}
				return nil
			}))

			var handlerCalls atomic.Int32
			var providerCalls atomic.Int32
			componentValue := &TaskDispatchComponent{calls: &handlerCalls}
			options := []component.Option{component.WithName("taskexecutor")}
			if testCase.receiverProvider {
				options = append(options, component.WithReceiverProvider(func(context.Context) component.Component {
					providerCalls.Add(1)
					return componentValue
				}))
			}
			componentService := component.NewService(componentValue, options)
			require.NoError(t, componentService.ExtractHandler())
			handler, ok := componentService.Handlers["Invoke"]
			require.True(t, ok)

			pool := NewHandlerPool()
			pool.Register("taskexecutor", "Invoke", handler)
			hooks := pipeline.NewHandlerHooks()
			var hookCalls atomic.Int32
			hooks.BeforeHandler.PushBack(func(
				ctx context.Context,
				_ route.Route,
				arg interface{},
			) (context.Context, interface{}, error) {
				hookCalls.Add(1)
				if testCase.receiverProvider {
					return context.Background(), arg, nil
				}
				return ctx, arg, nil
			})
			data, err := proto.Marshal(&test.SomeStruct{B: "queued"})
			require.NoError(t, err)
			ctrl := gomock.NewController(t)
			sess := sessionmocks.NewMockSession(ctrl)
			sess.EXPECT().UID().Return("user-1").AnyTimes()
			sess.EXPECT().ID().Return(int64(1)).AnyTimes()

			_, err = pool.ProcessHandlerMessage(
				ctx,
				route.NewRoute("", "taskexecutor", "Invoke"),
				protobufserializer.NewSerializer(),
				hooks,
				sess,
				data,
				message.Request,
				true,
			)
			assert.ErrorIs(t, err, constants.ErrStaleOwnerEpoch)
			assert.True(t, IsRPCAdmissionRejected(err))
			assert.Equal(t, testCase.rejectAt, validateCalls.Load())
			assert.Equal(t, testCase.wantHookCalls, hookCalls.Load())
			assert.Zero(t, providerCalls.Load(), "the receiver provider must not start after admission rejection")
			assert.Zero(t, handlerCalls.Load())
		})
	}
}

func TestRPCFingerprintIgnoresStatefulOwnerHopMetadata(t *testing.T) {
	const key = "command-1"
	metadata := func(assignment string, epoch string) []byte {
		encoded, err := pcontext.Encode(pcontext.AddListToPropagateCtx(
			context.Background(),
			constants.IdempotencyKey, key,
			constants.StatefulOwnerKey, assignment,
			constants.StatefulOwnerEpochKey, epoch,
			"tenant", "east",
		))
		require.NoError(t, err)
		return encoded
	}
	request := &protos.Request{
		Type:     protos.RPCType_User,
		Msg:      &protos.Msg{Route: "game.player.move", Data: []byte("north")},
		Metadata: metadata(strings.Repeat("a", 64), "73"),
	}
	first, err := rpcRequestFingerprint(request, key, false, "")
	require.NoError(t, err)
	request.Metadata = metadata(strings.Repeat("b", 64), "99")
	retry, err := rpcRequestFingerprint(request, key, false, "")
	require.NoError(t, err)
	assert.Equal(t, first, retry)

	digest := strings.Repeat("c", 64)
	reliableMetadata := func(assignment string, epoch string) []byte {
		encoded, err := pcontext.Encode(pcontext.AddListToPropagateCtx(
			context.Background(),
			constants.IdempotencyKey, key,
			constants.StatefulOwnerKey, assignment,
			constants.StatefulOwnerEpochKey, epoch,
			rpccontext.ReliableJobKey, key,
			rpccontext.ReliableJobMetadataDigestKey, digest,
			"tenant", "east",
		))
		require.NoError(t, err)
		return encoded
	}
	request.Metadata = reliableMetadata(strings.Repeat("a", 64), "73")
	first, err = rpcRequestFingerprint(request, key, true, digest)
	require.NoError(t, err)
	request.Metadata = reliableMetadata(strings.Repeat("b", 64), "99")
	retry, err = rpcRequestFingerprint(request, key, true, digest)
	require.NoError(t, err)
	assert.Equal(t, first, retry)
}
