package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

type CallCoverageRemote struct {
	component.Base
	calls *atomic.Int32
}

type CascadingCallRemote struct {
	component.Base
	downstreams []*RemoteService
	markers     chan any
	keys        chan string
}

type IndependentNestedCommandsRemote struct {
	component.Base
	downstream *RemoteService
	keys       chan string
}

func (r *CascadingCallRemote) Forward(ctx context.Context, input *test.SomeStruct) (*test.SomeStruct, error) {
	r.markers <- pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey)
	key, _ := pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyKey).(string)
	r.keys <- key
	data, err := proto.Marshal(input)
	if err != nil {
		return nil, err
	}
	var response *protos.Response
	for _, downstream := range r.downstreams {
		response, err = downstream.Call(ctx, &protos.Request{
			Type: protos.RPCType_User,
			Msg:  &protos.Msg{Route: "CallCoverageRemote.Echo", Data: data},
		})
		if err != nil {
			return nil, err
		}
	}
	result := &test.SomeStruct{}
	if err := proto.Unmarshal(response.GetData(), result); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *IndependentNestedCommandsRemote) Forward(ctx context.Context, input *test.SomeStruct) (*test.SomeStruct, error) {
	key, _ := pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyKey).(string)
	r.keys <- key
	for _, value := range []string{"reserve", "audit"} {
		data, err := proto.Marshal(&test.SomeStruct{B: value})
		if err != nil {
			return nil, err
		}
		if _, err := r.downstream.Call(ctx, &protos.Request{
			Type: protos.RPCType_User,
			Msg:  &protos.Msg{Route: "CallCoverageRemote.Echo", Data: data},
		}); err != nil {
			return nil, err
		}
	}
	return &test.SomeStruct{B: input.GetB()}, nil
}

func (r *CallCoverageRemote) Echo(_ context.Context, input *test.SomeStruct) (*test.SomeStruct, error) {
	if r.calls != nil {
		r.calls.Add(1)
	}
	return &test.SomeStruct{B: input.GetB()}, nil
}

func (r *CallCoverageRemote) SessionUID(ctx context.Context) (*test.SomeStruct, error) {
	sess, _ := ctx.Value(constants.SessionCtxKey).(session.Session)
	if sess == nil {
		return nil, errors.New("session missing from remote context")
	}
	return &test.SomeStruct{B: sess.UID()}, nil
}

type recordingIdempotencyStore struct {
	calls       int
	key         string
	fingerprint string
	invoke      bool
	response    *protos.Response
	err         error
}

func (s *recordingIdempotencyStore) Execute(
	ctx context.Context,
	key string,
	fingerprint string,
	fn func(context.Context) (*protos.Response, error),
) (*protos.Response, error) {
	s.calls++
	s.key = key
	s.fingerprint = fingerprint
	if s.invoke {
		return fn(ctx)
	}
	return s.response, s.err
}

func newRemoteCallService(server *cluster.Server, opts ...RemoteServiceOption) *RemoteService {
	return NewRemoteServiceWithOptions(
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		server,
		session.NewSessionPool(),
		pipeline.NewRemoteHooks(),
		pipeline.NewHandlerHooks(),
		NewHandlerPool(),
		opts...,
	)
}

func callCoverageRequest(t *testing.T, value string) *protos.Request {
	t.Helper()
	data, err := proto.Marshal(&test.SomeStruct{B: value})
	require.NoError(t, err)
	return &protos.Request{
		Type: protos.RPCType_User,
		Msg: &protos.Msg{
			Route: "CallCoverageRemote.Echo",
			Data:  data,
		},
	}
}

func decodeCallCoverageResponse(t *testing.T, response *protos.Response) *test.SomeStruct {
	t.Helper()
	require.NotNil(t, response)
	require.Nil(t, response.Status)
	result := &test.SomeStruct{}
	require.NoError(t, proto.Unmarshal(response.Data, result))
	return result
}

func TestRemoteServiceCallProcessesAndDeduplicatesUserRequests(t *testing.T) {
	var calls atomic.Int32
	service := newRemoteCallService(cluster.NewServer("game-1", "game", false))
	require.NoError(t, service.Register(&CallCoverageRemote{calls: &calls}, nil))
	request := callCoverageRequest(t, "north")

	for range 2 {
		response, err := service.Call(context.Background(), request)
		require.NoError(t, err)
		assert.Equal(t, "north", decodeCallCoverageResponse(t, response).B)
	}
	assert.Equal(t, int32(2), calls.Load(), "requests without an idempotency key execute independently")

	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, "move-1")
	first, err := service.Call(ctx, request)
	require.NoError(t, err)
	require.NotEmpty(t, first.Data)
	first.Data[0] ^= 0xff

	second, err := service.Call(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, "north", decodeCallCoverageResponse(t, second).B)
	assert.Equal(t, int32(3), calls.Load(), "a repeated command must reuse the stored result")

	_, err = service.Call(ctx, callCoverageRequest(t, "south"))
	assert.ErrorIs(t, err, constants.ErrIdempotencyConflict)
	assert.Equal(t, int32(3), calls.Load(), "a conflicting command must not execute")

	reservedPrefixCtx := pcontext.AddToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey,
		"pitaya-parent-v1:external-command",
	)
	_, err = service.Call(reservedPrefixCtx, callCoverageRequest(t, "prefix-first"))
	require.NoError(t, err)
	_, err = service.Call(reservedPrefixCtx, callCoverageRequest(t, "prefix-conflict"))
	assert.ErrorIs(t, err, constants.ErrIdempotencyConflict)
}

func TestRemoteServiceCallReportsRoutingAndRPCTypeErrors(t *testing.T) {
	service := newRemoteCallService(cluster.NewServer("game-1", "game", false))

	response, err := service.Call(context.Background(), &protos.Request{
		Type: protos.RPCType_User,
		Msg:  &protos.Msg{Route: "invalid"},
	})
	assert.Error(t, err)
	assert.Nil(t, response)

	response = processRemoteMessage(context.Background(), &protos.Request{
		Type: protos.RPCType_User,
		Msg:  &protos.Msg{Route: "invalid"},
	}, service)
	require.NotNil(t, response.Status)
	assert.EqualValues(t, http.StatusBadRequest, response.Status.Code)
	assert.Equal(t, "invalid", response.Status.Metadata["route"])

	response, err = service.Call(context.Background(), &protos.Request{
		Type: protos.RPCType(99),
		Msg:  &protos.Msg{Route: "missing.method"},
	})
	require.Error(t, err)
	require.NotNil(t, response.Status)
	assert.EqualValues(t, http.StatusBadRequest, response.Status.Code)
	assert.Equal(t, "invalid rpc type", response.Status.Message)

	response, err = service.Call(context.Background(), &protos.Request{
		Type: protos.RPCType_User,
		Msg:  &protos.Msg{Route: "missing.method"},
	})
	require.Error(t, err)
	require.NotNil(t, response.Status)
	assert.EqualValues(t, http.StatusNotFound, response.Status.Code)
}

func TestRemoteServiceCallUsesInjectedIdempotencyStore(t *testing.T) {
	store := &recordingIdempotencyStore{invoke: true}
	service := newRemoteCallService(
		cluster.NewServer("game-1", "game", false),
		WithIdempotencyStore(store),
	)
	require.NoError(t, service.Register(&CallCoverageRemote{}, nil))
	request := callCoverageRequest(t, "east")
	key := strings.Repeat("x", MaxIdempotencyKeyBytes)
	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, key)

	response, err := service.Call(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, "east", decodeCallCoverageResponse(t, response).B)
	assert.Equal(t, 1, store.calls)
	expectedStoreKey, err := idempotencyStoreKey(ctx, service.server, key)
	require.NoError(t, err)
	assert.Equal(t, expectedStoreKey, store.key)
	assert.LessOrEqual(t, len(store.key), MaxIdempotencyKeyBytes)
	fingerprint, err := rpcRequestFingerprint(request)
	require.NoError(t, err)
	assert.Equal(t, fingerprint, store.fingerprint)

	withDefault := newRemoteCallService(
		cluster.NewServer("game-2", "game", false),
		WithIdempotencyStore(nil),
	)
	assert.IsType(t, &MemoryIdempotencyStore{}, withDefault.idempotencyStore)

	shortCircuit := &recordingIdempotencyStore{response: &protos.Response{Data: []byte("stored")}}
	nilServer := newRemoteCallService(nil, WithIdempotencyStore(shortCircuit))
	response, err = nilServer.Call(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("stored"), response.Data)
	expectedStoreKey, err = idempotencyStoreKey(ctx, nil, key)
	require.NoError(t, err)
	assert.Equal(t, expectedStoreKey, shortCircuit.key)
}

func TestRemoteServiceCallScopesDurableDeduplicationForRoutedAndFanoutCalls(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	first := newRemoteCallService(
		cluster.NewServer("game-1", "game", false),
		WithIdempotencyStore(store),
	)
	second := newRemoteCallService(
		cluster.NewServer("game-2", "game", false),
		WithIdempotencyStore(store),
	)
	require.NoError(t, first.Register(&CallCoverageRemote{calls: &firstCalls}, nil))
	require.NoError(t, second.Register(&CallCoverageRemote{calls: &secondCalls}, nil))
	request := callCoverageRequest(t, "shared")

	routedCtx := pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, "routed-command")
	for _, remote := range []*RemoteService{first, second} {
		response, err := remote.Call(routedCtx, request)
		require.NoError(t, err)
		assert.Equal(t, "shared", decodeCallCoverageResponse(t, response).B)
	}
	assert.Equal(t, int32(1), firstCalls.Load())
	assert.Zero(t, secondCalls.Load(), "owner failover must reuse the server-type-scoped result")

	fanoutCtx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, "fanout-command",
		constants.IdempotencyFanoutKey, "1",
	)
	for range 2 {
		for _, remote := range []*RemoteService{first, second} {
			response, err := remote.Call(fanoutCtx, request)
			require.NoError(t, err)
			assert.Equal(t, "shared", decodeCallCoverageResponse(t, response).B)
		}
	}
	assert.Equal(t, int32(2), firstCalls.Load())
	assert.Equal(t, int32(1), secondCalls.Load(), "each fanout target must execute independently once")
}

func TestRemoteServiceCallConsumesAdmissionMetadataBeforeNestedCalls(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	var firstLeafCalls atomic.Int32
	var secondLeafCalls atomic.Int32
	firstLeaf := newRemoteCallService(
		cluster.NewServer("leaf-1", "leaf", false),
		WithIdempotencyStore(store),
	)
	secondLeaf := newRemoteCallService(
		cluster.NewServer("leaf-2", "leaf", false),
		WithIdempotencyStore(store),
	)
	require.NoError(t, firstLeaf.Register(&CallCoverageRemote{calls: &firstLeafCalls}, nil))
	require.NoError(t, secondLeaf.Register(&CallCoverageRemote{calls: &secondLeafCalls}, nil))

	markers := make(chan any, 2)
	keys := make(chan string, 2)
	firstBranchServer := cluster.NewServer("branch-1", "branch", false)
	firstBranchServer.SetOwnerEpoch(7)
	secondBranchServer := cluster.NewServer("branch-2", "branch", false)
	secondBranchServer.SetOwnerEpoch(8)
	firstBranch := newRemoteCallService(
		firstBranchServer,
		WithIdempotencyStore(store),
	)
	secondBranch := newRemoteCallService(
		secondBranchServer,
		WithIdempotencyStore(store),
	)
	require.NoError(t, firstBranch.Register(&CascadingCallRemote{
		downstreams: []*RemoteService{firstLeaf, secondLeaf},
		markers:     markers,
		keys:        keys,
	}, nil))
	require.NoError(t, secondBranch.Register(&CascadingCallRemote{
		downstreams: []*RemoteService{secondLeaf, firstLeaf},
		markers:     markers,
		keys:        keys,
	}, nil))

	data, err := proto.Marshal(&test.SomeStruct{B: "nested"})
	require.NoError(t, err)
	request := &protos.Request{
		Type: protos.RPCType_User,
		Msg:  &protos.Msg{Route: "CascadingCallRemote.Forward", Data: data},
	}
	branches := []*RemoteService{firstBranch, secondBranch}
	branchServers := []*cluster.Server{firstBranchServer, secondBranchServer}
	for index, branch := range branches {
		ctx := pcontext.AddListToPropagateCtx(
			context.Background(),
			constants.IdempotencyKey, "fanout-cascade",
			constants.IdempotencyFanoutKey, "1",
			constants.OwnerServerIDKey, branchServers[index].ID,
			constants.OwnerEpochKey, fmt.Sprintf("%d", branchServers[index].OwnerEpoch()),
		)
		response, err := branch.Call(ctx, request)
		require.NoError(t, err)
		assert.Equal(t, "nested", decodeCallCoverageResponse(t, response).B)
	}

	assert.Nil(t, <-markers)
	assert.Nil(t, <-markers)
	assert.Empty(t, <-keys)
	assert.Empty(t, <-keys)
	assert.Equal(t, int32(2), firstLeafCalls.Load())
	assert.Equal(t, int32(2), secondLeafCalls.Load(), "nested calls without an explicit key execute independently")
}

func TestRemoteServiceCallConsumesAdmissionKeyBeforeIndependentNestedCommands(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	var leafCalls atomic.Int32
	leaf := newRemoteCallService(
		cluster.NewServer("leaf-1", "leaf", false),
		WithIdempotencyStore(store),
	)
	require.NoError(t, leaf.Register(&CallCoverageRemote{calls: &leafCalls}, nil))

	parent := newRemoteCallService(
		cluster.NewServer("branch-1", "branch", false),
		WithIdempotencyStore(store),
	)
	handlerKeys := make(chan string, 1)
	require.NoError(t, parent.Register(&IndependentNestedCommandsRemote{downstream: leaf, keys: handlerKeys}, nil))
	data, err := proto.Marshal(&test.SomeStruct{B: "parent-result"})
	require.NoError(t, err)
	request := &protos.Request{
		Type: protos.RPCType_User,
		Msg:  &protos.Msg{Route: "IndependentNestedCommandsRemote.Forward", Data: data},
	}
	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, "parent-command")

	for range 2 {
		response, err := parent.Call(ctx, request)
		require.NoError(t, err)
		assert.Equal(t, "parent-result", decodeCallCoverageResponse(t, response).B)
	}
	assert.Empty(t, <-handlerKeys, "the current hop's idempotency key must not leak into the handler")
	assert.Equal(t, int32(2), leafCalls.Load(), "parent retry must not repeat either accepted nested command")
}

func TestRemoteServiceCallValidatesFenceAndIdempotencyKeyBeforeDispatch(t *testing.T) {
	server := cluster.NewServer("game-1", "game", false)
	server.SetOwnerEpoch(7)
	service := newRemoteCallService(server)
	require.NoError(t, service.Register(&CallCoverageRemote{}, nil))
	request := callCoverageRequest(t, "west")

	response, err := service.Call(context.Background(), request)
	require.ErrorIs(t, err, constants.ErrStaleOwnerEpoch)
	require.NotNil(t, response.Status)

	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, server.ID,
		constants.OwnerEpochKey, "7",
	)
	invalidKeyCtx := pcontext.AddToPropagateCtx(ctx, constants.IdempotencyKey, 42)
	response, err = service.Call(invalidKeyCtx, request)
	require.ErrorIs(t, err, constants.ErrIdempotencyKeyInvalid)
	require.NotNil(t, response.Status)

	validCtx := pcontext.AddToPropagateCtx(ctx, constants.IdempotencyKey, "move-7")
	response, err = service.Call(validCtx, request)
	require.NoError(t, err)
	assert.Equal(t, "west", decodeCallCoverageResponse(t, response).B)
}

func TestRemoteServiceCallInterceptorHandlesResultsErrorsAndSessions(t *testing.T) {
	t.Run("protobuf result", func(t *testing.T) {
		service := newRemoteCallService(cluster.NewServer("game-1", "game", false))
		service.RegisterInterceptor("gate", &component.Interceptor{InterceptorFun: func(
			_ context.Context,
			rt route.Route,
			data []byte,
		) (proto.Message, error) {
			assert.Equal(t, "gate.echo", rt.Short())
			assert.Equal(t, []byte("payload"), data)
			return &test.SomeStruct{B: "accepted"}, nil
		}})

		response, err := service.Call(context.Background(), &protos.Request{
			Type: protos.RPCType_User,
			Msg:  &protos.Msg{Route: "gate.echo", Data: []byte("payload")},
		})
		require.NoError(t, err)
		assert.Equal(t, "accepted", decodeCallCoverageResponse(t, response).B)
	})

	t.Run("nil result", func(t *testing.T) {
		service := newRemoteCallService(cluster.NewServer("game-1", "game", false))
		service.RegisterInterceptor("gate", &component.Interceptor{InterceptorFun: func(
			context.Context,
			route.Route,
			[]byte,
		) (proto.Message, error) {
			return nil, nil
		}})

		response, err := service.Call(context.Background(), &protos.Request{
			Type: protos.RPCType_User,
			Msg:  &protos.Msg{Route: "gate.echo"},
		})
		require.NoError(t, err)
		assert.Empty(t, response.Data)
	})

	t.Run("error", func(t *testing.T) {
		sentinel := errors.New("interceptor rejected request")
		service := newRemoteCallService(cluster.NewServer("game-1", "game", false))
		service.RegisterInterceptor("gate", &component.Interceptor{InterceptorFun: func(
			context.Context,
			route.Route,
			[]byte,
		) (proto.Message, error) {
			return nil, sentinel
		}})

		response, err := service.Call(context.Background(), &protos.Request{
			Type: protos.RPCType_User,
			Msg:  &protos.Msg{Route: "gate.echo"},
		})
		require.ErrorContains(t, err, sentinel.Error())
		require.NotNil(t, response.Status)
	})
}

func TestRemoteServiceCallAddsRemoteSessionToContext(t *testing.T) {
	ctrl := gomock.NewController(t)
	discovery := clustermocks.NewMockServiceDiscovery(ctrl)
	discovery.EXPECT().GetSelfServer().Return(&cluster.Server{Frontend: true}).Times(2)
	pool := session.NewSessionPool()
	service := NewRemoteService(
		nil,
		nil,
		discovery,
		nil,
		nil,
		nil,
		nil,
		cluster.NewServer("game-1", "game", false),
		pool,
		pipeline.NewRemoteHooks(),
		pipeline.NewHandlerHooks(),
		NewHandlerPool(),
	)
	require.NoError(t, service.Register(&CallCoverageRemote{}, nil))
	service.RegisterInterceptor("gate", &component.Interceptor{InterceptorFun: func(
		ctx context.Context,
		_ route.Route,
		_ []byte,
	) (proto.Message, error) {
		sess, ok := ctx.Value(constants.SessionCtxKey).(session.Session)
		require.True(t, ok)
		return &test.SomeStruct{B: sess.UID()}, nil
	}})

	requestSession := &protos.Session{Id: 17, Uid: "user-17"}
	response, err := service.Call(context.Background(), &protos.Request{
		Type:    protos.RPCType_User,
		Session: requestSession,
		Msg:     &protos.Msg{Route: "gate.session"},
	})
	require.NoError(t, err)
	assert.Equal(t, requestSession.Uid, decodeCallCoverageResponse(t, response).B)

	response, err = service.Call(context.Background(), &protos.Request{
		Type:    protos.RPCType_User,
		Session: requestSession,
		Msg:     &protos.Msg{Route: "CallCoverageRemote.SessionUID"},
	})
	require.NoError(t, err)
	assert.Equal(t, requestSession.Uid, decodeCallCoverageResponse(t, response).B)
}

func TestRemoteServiceCallReportsInvalidRemoteSessionData(t *testing.T) {
	ctrl := gomock.NewController(t)
	discovery := clustermocks.NewMockServiceDiscovery(ctrl)
	discovery.EXPECT().GetSelfServer().Return(&cluster.Server{Frontend: false})
	service := NewRemoteService(
		nil,
		nil,
		discovery,
		nil,
		nil,
		nil,
		nil,
		cluster.NewServer("game-1", "game", false),
		session.NewSessionPool(),
		pipeline.NewRemoteHooks(),
		pipeline.NewHandlerHooks(),
		NewHandlerPool(),
	)
	require.NoError(t, service.Register(&CallCoverageRemote{}, nil))

	response, err := service.Call(context.Background(), &protos.Request{
		Type: protos.RPCType_User,
		Session: &protos.Session{
			Uid:  "user-bad-data",
			Data: []byte("{invalid"),
		},
		Msg: &protos.Msg{Route: "CallCoverageRemote.SessionUID"},
	})
	require.Error(t, err)
	require.NotNil(t, response.Status)
}

func TestRemoteServiceCallUsesReceiverProvider(t *testing.T) {
	t.Run("missing receiver", func(t *testing.T) {
		service := newRemoteCallService(cluster.NewServer("game-1", "game", false))
		require.NoError(t, service.Register(
			&CallCoverageRemote{},
			[]component.Option{component.WithReceiverProvider(func(context.Context) component.Component { return nil })},
		))

		response, err := service.Call(context.Background(), callCoverageRequest(t, "value"))
		require.Error(t, err)
		require.NotNil(t, response.Status)
		assert.EqualValues(t, http.StatusNotFound, response.Status.Code)
		assert.Contains(t, response.Status.Message, "ReceiverProvider return nil")
	})

	t.Run("dynamic receiver", func(t *testing.T) {
		var calls atomic.Int32
		service := newRemoteCallService(cluster.NewServer("game-1", "game", false))
		require.NoError(t, service.Register(
			&CallCoverageRemote{},
			[]component.Option{component.WithReceiverProvider(func(context.Context) component.Component {
				return &CallCoverageRemote{calls: &calls}
			})},
		))

		response, err := service.Call(context.Background(), callCoverageRequest(t, "value"))
		require.NoError(t, err)
		assert.Equal(t, "value", decodeCallCoverageResponse(t, response).B)
		assert.Equal(t, int32(1), calls.Load())
	})
}

func TestRemoteServiceFanoutWrappersSelectNotifyOrRequest(t *testing.T) {
	ctrl := gomock.NewController(t)
	discovery := clustermocks.NewMockServiceDiscovery(ctrl)
	discovery.EXPECT().GetServersByType("room").Return(map[string]*cluster.Server{}, nil).Times(2)
	discovery.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{}).Times(2)
	service := NewRemoteService(
		nil,
		nil,
		discovery,
		nil,
		nil,
		nil,
		nil,
		cluster.NewServer("room-1", "room", false),
		nil,
		nil,
		nil,
		nil,
	)
	rt := route.NewRoute("room", "table", "sync")

	responses, err := service.Fork(context.Background(), false, rt, &test.SomeStruct{B: "request"}, nil)
	require.NoError(t, err)
	require.NotNil(t, responses)
	assert.Empty(t, responses)

	responses, err = service.Fork(context.Background(), true, rt, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, responses)

	responses, err = service.Publish(context.Background(), rt, false, &test.SomeStruct{B: "request"}, nil)
	require.NoError(t, err)
	require.NotNil(t, responses)
	assert.Empty(t, responses)

	responses, err = service.Publish(context.Background(), rt, true, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, responses)
}

func TestRemoteServiceNotifySerializesAndTargetsServer(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		discovery := clustermocks.NewMockServiceDiscovery(ctrl)
		rpcClient := clustermocks.NewMockRPCClient(ctrl)
		target := cluster.NewServer("game-2", "game", false)
		rt := route.NewRoute("game", "player", "sync")
		ctx := context.Background()
		discovery.EXPECT().GetServer(target.ID).Return(target, nil)
		rpcClient.EXPECT().Call(
			ctx,
			protos.RPCType_User,
			rt,
			nil,
			gomock.Any(),
			target,
		).DoAndReturn(func(
			_ context.Context,
			_ protos.RPCType,
			_ *route.Route,
			_ session.Session,
			msg *message.Message,
			_ *cluster.Server,
		) (*protos.Response, error) {
			assert.Equal(t, message.Notify, msg.Type)
			assert.Equal(t, rt.Short(), msg.Route)
			decoded := &test.SomeStruct{}
			require.NoError(t, proto.Unmarshal(msg.Data, decoded))
			assert.Equal(t, "payload", decoded.B)
			return &protos.Response{}, nil
		})

		service := NewRemoteService(
			rpcClient, nil, discovery, nil, nil, nil, nil,
			cluster.NewServer("game-1", "game", false), nil, nil, nil, nil,
		)
		require.NoError(t, service.Notify(ctx, target.ID, rt, &test.SomeStruct{B: "payload"}, nil))
	})

	t.Run("missing target", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		discovery := clustermocks.NewMockServiceDiscovery(ctrl)
		discovery.EXPECT().GetServer("missing").Return(nil, nil)
		service := NewRemoteService(
			nil, nil, discovery, nil, nil, nil, nil,
			cluster.NewServer("game-1", "game", false), nil, nil, nil, nil,
		)

		err := service.Notify(
			context.Background(),
			"missing",
			route.NewRoute("game", "player", "sync"),
			nil,
			nil,
		)
		assert.ErrorIs(t, err, constants.ErrServerNotFound)
	})
}
