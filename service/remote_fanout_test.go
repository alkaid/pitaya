package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/router"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type remoteServiceConstructor func(
	cluster.RPCClient,
	cluster.RPCServer,
	cluster.ServiceDiscovery,
	codec.PacketEncoder,
	serialize.Serializer,
	*router.Router,
	message.Encoder,
	*cluster.Server,
	session.SessionPool,
	*pipeline.RemoteHooks,
	*pipeline.HandlerHooks,
	*HandlerPool,
) *RemoteService

var _ remoteServiceConstructor = NewRemoteService

type fanoutRPCClient struct {
	call func(context.Context, protos.RPCType, *route.Route, session.Session, *message.Message, *cluster.Server) (*protos.Response, error)
	fork func(context.Context, *route.Route, session.Session, *message.Message) error
}

func (c *fanoutRPCClient) Send(string, []byte) error { return nil }

func (c *fanoutRPCClient) SendPush(string, *cluster.Server, *protos.Push) error { return nil }

func (c *fanoutRPCClient) SendKick(string, string, *protos.KickMsg) error { return nil }

func (c *fanoutRPCClient) BroadcastSessionBind(string) error { return nil }

func (c *fanoutRPCClient) Call(
	ctx context.Context,
	rpcType protos.RPCType,
	route *route.Route,
	session session.Session,
	msg *message.Message,
	server *cluster.Server,
) (*protos.Response, error) {
	return c.call(ctx, rpcType, route, session, msg, server)
}

func (c *fanoutRPCClient) Fork(ctx context.Context, route *route.Route, session session.Session, msg *message.Message) error {
	return c.fork(ctx, route, session, msg)
}

func (c *fanoutRPCClient) Publish(
	context.Context,
	protos.RPCType,
	*route.Route,
	session.Session,
	*message.Message,
	int,
	...time.Duration,
) ([]*protos.Response, error) {
	return nil, nil
}

func (c *fanoutRPCClient) Init() error     { return nil }
func (c *fanoutRPCClient) AfterInit()      {}
func (c *fanoutRPCClient) BeforeShutdown() {}
func (c *fanoutRPCClient) Shutdown() error { return nil }

type fanoutCallOutcome struct {
	responses []*protos.Response
	err       error
}

func receiveFanoutValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fanout test signal")
		var zero T
		return zero
	}
}

func newSubscriberServer(id, serverType, service, method string, fork bool) *cluster.Server {
	server := &cluster.Server{
		ID:        id,
		Type:      serverType,
		Subscribe: make(map[string]*cluster.SubscribeItem),
	}
	server.AddSubscribe(service, method, fork)
	return server
}

func TestDoForkCrossTypeRoutesPerTargetWithOwnerEpoch(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	targets := map[string]*cluster.Server{
		"gateway-1": {ID: "gateway-1", Type: "room"},
		"room-b":    {ID: "room-b", Type: "room"},
	}
	targets["gateway-1"].SetOwnerEpoch(41)
	targets["room-b"].SetOwnerEpoch(42)
	sd.EXPECT().GetServersByType("room").Return(targets, nil)

	var mu sync.Mutex
	seen := make(map[string]string, len(targets))
	var legacyForkCalled atomic.Bool
	rpcClient := &fanoutRPCClient{
		call: func(
			ctx context.Context,
			rpcType protos.RPCType,
			calledRoute *route.Route,
			_ session.Session,
			msg *message.Message,
			server *cluster.Server,
		) (*protos.Response, error) {
			assert.Equal(t, protos.RPCType_User, rpcType)
			assert.Equal(t, "room.table.sync", calledRoute.String())
			assert.Equal(t, message.Notify, msg.Type)
			assert.Equal(t, "table.sync", msg.Route)
			assert.Equal(t, server.ID, pcontext.GetFromPropagateCtx(ctx, constants.OwnerServerIDKey))

			mu.Lock()
			seen[server.ID] = pcontext.GetFromPropagateCtx(ctx, constants.OwnerEpochKey).(string)
			mu.Unlock()
			return &protos.Response{}, nil
		},
		fork: func(context.Context, *route.Route, session.Session, *message.Message) error {
			legacyForkCalled.Store(true)
			return errors.New("type-wide fork cannot carry a target owner epoch")
		},
	}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "gateway-1", Type: "gateway"}, nil, nil, nil, nil,
	)

	err := svc.DoFork(
		context.Background(),
		route.NewRoute("room", "table", "sync"),
		nil,
		nil,
	)

	require.NoError(t, err)
	assert.False(t, legacyForkCalled.Load())
	assert.Equal(t, map[string]string{"gateway-1": "41", "room-b": "42"}, seen)
}

func TestDoForkRequestIncludesAllTargetsWhenSelfIsAbsent(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	rpcClient := clustermocks.NewMockRPCClient(ctrl)
	servers := map[string]*cluster.Server{
		"b": {ID: "b", Type: "room"},
		"a": {ID: "a", Type: "room"},
	}
	sd.EXPECT().GetServersByType("room").Return(servers, nil)

	var started atomic.Int32
	allStarted := make(chan struct{})
	rpcClient.EXPECT().Call(
		gomock.Any(),
		protos.RPCType_User,
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).DoAndReturn(func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		if started.Add(1) == int32(len(servers)) {
			close(allStarted)
		}
		<-allStarted
		return &protos.Response{Data: []byte(server.ID)}, nil
	}).Times(len(servers))

	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "room"}, nil, nil, nil, nil,
	)
	responses, err := svc.DoForkRequest(
		context.Background(),
		route.NewRoute("room", "table", "sync"),
		nil,
		nil,
	)

	require.NoError(t, err)
	require.Len(t, responses, 2)
	assert.Equal(t, "a", string(responses[0].Data))
	assert.Equal(t, "b", string(responses[1].Data))
}

func TestDoForkRequestReturnsNonNilEmptyResponsesWhenOnlySelfExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	self := &cluster.Server{ID: "self", Type: "room"}
	sd.EXPECT().GetServersByType("room").Return(map[string]*cluster.Server{"self": self}, nil)

	svc := NewRemoteService(nil, nil, sd, nil, nil, nil, nil, self, nil, nil, nil, nil)
	responses, err := svc.DoForkRequest(
		context.Background(),
		route.NewRoute("room", "table", "sync"),
		nil,
		nil,
	)

	require.NoError(t, err)
	require.NotNil(t, responses)
	assert.Empty(t, responses)
}

func TestDoForkRequestReturnsOneTarget(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	target := &cluster.Server{ID: "target", Type: "room"}
	sd.EXPECT().GetServersByType("room").Return(map[string]*cluster.Server{"target": target}, nil)

	marker := make(chan any, 1)
	rpcClient := &fanoutRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		marker <- pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey)
		return &protos.Response{Data: []byte(server.ID)}, nil
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "room"}, nil, nil, nil, nil,
	)

	responses, err := svc.DoForkRequest(
		context.Background(),
		route.NewRoute("room", "table", "sync"),
		nil,
		nil,
	)

	require.NoError(t, err)
	require.Len(t, responses, 1)
	assert.Equal(t, "target", string(responses[0].Data))
	assert.Equal(t, "1", receiveFanoutValue(t, marker))
}

func TestDoForkRequestReturnsStablePartialResponsesAndJoinedErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	servers := map[string]*cluster.Server{
		"self": {ID: "self", Type: "room"},
		"d":    {ID: "d", Type: "room"},
		"b":    {ID: "b", Type: "room"},
		"a":    {ID: "a", Type: "room"},
		"c":    {ID: "c", Type: "room"},
	}
	sd.EXPECT().GetServersByType("room").Return(servers, nil)

	errB := errors.New("b failed")
	errC := errors.New("c failed")
	started := make(chan string, 4)
	finished := make(chan string, 4)
	release := map[string]chan struct{}{
		"a": make(chan struct{}),
		"b": make(chan struct{}),
		"c": make(chan struct{}),
		"d": make(chan struct{}),
	}
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		started <- server.ID
		<-release[server.ID]
		finished <- server.ID
		switch server.ID {
		case "b":
			return nil, errB
		case "c":
			return nil, errC
		default:
			return &protos.Response{Data: []byte(server.ID)}, nil
		}
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		servers["self"], nil, nil, nil, nil,
	)

	result := make(chan fanoutCallOutcome, 1)
	go func() {
		responses, err := svc.DoForkRequest(
			context.Background(),
			route.NewRoute("room", "table", "sync"),
			nil,
			nil,
		)
		result <- fanoutCallOutcome{responses: responses, err: err}
	}()

	for range release {
		receiveFanoutValue(t, started)
	}
	for _, serverID := range []string{"d", "c", "b", "a"} {
		close(release[serverID])
		assert.Equal(t, serverID, receiveFanoutValue(t, finished))
	}

	outcome := receiveFanoutValue(t, result)
	require.Len(t, outcome.responses, 2)
	assert.Equal(t, "a", string(outcome.responses[0].Data))
	assert.Equal(t, "d", string(outcome.responses[1].Data))
	require.Error(t, outcome.err)
	assert.ErrorIs(t, outcome.err, errB)
	assert.ErrorIs(t, outcome.err, errC)
	assert.ErrorContains(t, outcome.err, "server b")
	assert.ErrorContains(t, outcome.err, "server c")
}

func TestDoForkRequestAllErrorsReturnNonNilEmptyResponses(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	servers := map[string]*cluster.Server{
		"a": {ID: "a", Type: "room"},
		"b": {ID: "b", Type: "room"},
	}
	sd.EXPECT().GetServersByType("room").Return(servers, nil)

	errA := errors.New("a failed")
	errB := errors.New("b failed")
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		if server.ID == "a" {
			return nil, errA
		}
		return nil, errB
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "room"}, nil, nil, nil, nil,
	)

	responses, err := svc.DoForkRequest(
		context.Background(),
		route.NewRoute("room", "table", "sync"),
		nil,
		nil,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, errA)
	assert.ErrorIs(t, err, errB)
	require.NotNil(t, responses)
	assert.Empty(t, responses)
}

func TestDoForkRequestConvertsTargetPanicToError(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	servers := map[string]*cluster.Server{
		"a": {ID: "a", Type: "room"},
		"b": {ID: "b", Type: "room"},
	}
	sd.EXPECT().GetServersByType("room").Return(servers, nil)

	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		if server.ID == "a" {
			panic("invalid propagated timeout")
		}
		return &protos.Response{Data: []byte(server.ID)}, nil
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "room"}, nil, nil, nil, nil,
	)

	responses, err := svc.DoForkRequest(
		context.Background(),
		route.NewRoute("room", "table", "sync"),
		nil,
		nil,
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "server a")
	assert.ErrorContains(t, err, errRemoteFanoutPanic.Error())
	assert.NotContains(t, err.Error(), "invalid propagated timeout")
	require.Len(t, responses, 1)
	assert.Equal(t, "b", string(responses[0].Data))
}

func TestDoForkRequestCancellationStopsNewCallsAndWaitsForStartedCalls(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	servers := map[string]*cluster.Server{
		"c": {ID: "c", Type: "room"},
		"a": {ID: "a", Type: "room"},
		"b": {ID: "b", Type: "room"},
	}
	sd.EXPECT().GetServersByType("room").Return(servers, nil)

	started := make(chan string, len(servers))
	sawCancellation := make(chan struct{})
	releaseStartedCall := make(chan struct{})
	var cancellationOnce sync.Once
	rpcClient := &fanoutRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		started <- server.ID
		if server.ID != "a" {
			return nil, errors.New("call started after concurrency limit")
		}
		<-ctx.Done()
		cancellationOnce.Do(func() { close(sawCancellation) })
		<-releaseStartedCall
		return nil, ctx.Err()
	}}
	svc := NewRemoteServiceWithOptions(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "room"}, nil, nil, nil, nil,
		WithFanoutConcurrency(1),
	)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan fanoutCallOutcome, 1)
	go func() {
		responses, err := svc.DoForkRequest(
			ctx,
			route.NewRoute("room", "table", "sync"),
			nil,
			nil,
		)
		result <- fanoutCallOutcome{responses: responses, err: err}
	}()

	assert.Equal(t, "a", receiveFanoutValue(t, started))
	cancel()
	receiveFanoutValue(t, sawCancellation)
	select {
	case <-result:
		t.Fatal("fanout returned before its started RPC completed")
	default:
	}

	close(releaseStartedCall)
	outcome := receiveFanoutValue(t, result)
	require.Error(t, outcome.err)
	assert.ErrorIs(t, outcome.err, context.Canceled)
	assert.ErrorContains(t, outcome.err, "server a")
	assert.ErrorContains(t, outcome.err, "server b")
	assert.Empty(t, outcome.responses)
	assert.Empty(t, started, "no additional target should start after cancellation")
}

func TestRemoteServiceFanoutConcurrencyOption(t *testing.T) {
	defaultService := NewRemoteService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	assert.Equal(t, constants.DefaultRPCFanoutConcurrency, defaultService.fanout.concurrency)

	configuredService := NewRemoteServiceWithOptions(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		WithFanoutConcurrency(7),
	)
	assert.Equal(t, 7, configuredService.fanout.concurrency)

	nonPositiveService := NewRemoteServiceWithOptions(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		WithFanoutConcurrency(0),
	)
	assert.Equal(t, constants.DefaultRPCFanoutConcurrency, nonPositiveService.fanout.concurrency)
}

func TestDoPublishPreservesRouteAndJoinsErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	alpha := newSubscriberServer("alpha-1", "alpha", "events", "refresh", true)
	beta := newSubscriberServer("beta-1", "beta", "events", "refresh", true)
	alpha.SetOwnerEpoch(51)
	beta.SetOwnerEpoch(52)
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{
		"beta":  beta,
		"alpha": alpha,
	})
	sd.EXPECT().GetServersByType("alpha").Return(map[string]*cluster.Server{alpha.ID: alpha}, nil)
	sd.EXPECT().GetServersByType("beta").Return(map[string]*cluster.Server{beta.ID: beta}, nil)

	errAlpha := errors.New("alpha failed")
	errBeta := errors.New("beta failed")
	calledRoutes := make([]string, 0, 2)
	markers := make([]any, 0, 2)
	owners := make([]string, 0, 2)
	rpcClient := &fanoutRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		route *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		calledRoutes = append(calledRoutes, route.String())
		markers = append(markers, pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey))
		owners = append(owners, fmt.Sprintf(
			"%s:%s",
			pcontext.GetFromPropagateCtx(ctx, constants.OwnerServerIDKey),
			pcontext.GetFromPropagateCtx(ctx, constants.OwnerEpochKey),
		))
		if route.SvType == "alpha" {
			return nil, errAlpha
		}
		assert.Equal(t, beta, server)
		return nil, errBeta
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
	)
	originalRoute := route.NewRoute("original", "events", "refresh")

	err := svc.DoPublish(context.Background(), originalRoute, nil, nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, errAlpha)
	assert.ErrorIs(t, err, errBeta)
	assert.Equal(t, []string{"alpha.events.refresh", "beta.events.refresh"}, calledRoutes)
	assert.Equal(t, []any{"1", "1"}, markers)
	assert.Equal(t, []string{"alpha-1:51", "beta-1:52"}, owners)
	assert.Equal(t, &route.Route{SvType: "original", Service: "events", Method: "refresh"}, originalRoute)
}

func TestNotifyAllReturnsStableJoinedTargetErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	self := &cluster.Server{ID: "self", Type: "gateway"}
	alpha := &cluster.Server{ID: "alpha-1", Type: "alpha"}
	beta := &cluster.Server{ID: "beta-1", Type: "beta"}
	gamma := &cluster.Server{ID: "gamma-1", Type: "gamma"}
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{
		"gamma":   gamma,
		"gateway": self,
		"beta":    beta,
		"alpha":   alpha,
	})
	sd.EXPECT().GetServersByType("alpha").Return(map[string]*cluster.Server{alpha.ID: alpha}, nil)
	sd.EXPECT().GetServersByType("beta").Return(map[string]*cluster.Server{beta.ID: beta}, nil)
	sd.EXPECT().GetServersByType("gamma").Return(map[string]*cluster.Server{gamma.ID: gamma}, nil)

	errAlpha := errors.New("alpha failed")
	errBeta := errors.New("beta failed")
	started := make(chan string, 3)
	release := map[string]chan struct{}{
		alpha.ID: make(chan struct{}),
		beta.ID:  make(chan struct{}),
		gamma.ID: make(chan struct{}),
	}
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		started <- server.ID
		<-release[server.ID]
		switch server.ID {
		case alpha.ID:
			return nil, errAlpha
		case beta.ID:
			return nil, errBeta
		default:
			return &protos.Response{}, nil
		}
	}}
	rpcRouter := router.New()
	rpcRouter.SetServiceDiscovery(sd)
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, rpcRouter, nil,
		self, nil, nil, nil, nil,
	)

	result := make(chan error, 1)
	go func() {
		result <- svc.NotifyAll(
			context.Background(),
			route.NewRoute("", "events", "refresh"),
			nil,
			nil,
		)
	}()
	for range release {
		receiveFanoutValue(t, started)
	}
	close(release[beta.ID])
	close(release[gamma.ID])
	close(release[alpha.ID])

	err := receiveFanoutValue(t, result)
	require.Error(t, err)
	assert.ErrorIs(t, err, errAlpha)
	assert.ErrorIs(t, err, errBeta)
	assert.Equal(t, "server alpha-1: alpha failed\nserver beta-1: beta failed", err.Error())
}

func TestPublishZeroTargetsIgnoreCanceledContext(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(fmt.Sprintf("fork_%t", fork), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			sd := clustermocks.NewMockServiceDiscovery(ctrl)
			self := newSubscriberServer("self", "gateway", "events", "refresh", fork)
			sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{"gateway": self}).Times(2)
			if fork {
				sd.EXPECT().GetServersByType("gateway").Return(map[string]*cluster.Server{"self": self}, nil).Times(2)
			}
			svc := NewRemoteService(nil, nil, sd, nil, nil, nil, nil, self, nil, nil, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			targetRoute := route.NewRoute("", "events", "refresh")

			require.NoError(t, svc.DoPublish(ctx, targetRoute, nil, nil))
			responses, err := svc.DoPublishRequest(ctx, targetRoute, nil, nil)
			require.NoError(t, err)
			require.NotNil(t, responses)
			assert.Empty(t, responses)
		})
	}
}

func TestDoPublishRequestUsesOneLimitAcrossForkTypes(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	alpha := newSubscriberServer("alpha-representative", "alpha", "events", "refresh", true)
	beta := newSubscriberServer("beta-representative", "beta", "events", "refresh", true)
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{
		"beta":  beta,
		"alpha": alpha,
	})
	sd.EXPECT().GetServersByType("alpha").Return(map[string]*cluster.Server{
		"a3": {ID: "a3", Type: "alpha"},
		"a1": {ID: "a1", Type: "alpha"},
		"a2": {ID: "a2", Type: "alpha"},
	}, nil)
	sd.EXPECT().GetServersByType("beta").Return(map[string]*cluster.Server{
		"b2": {ID: "b2", Type: "beta"},
		"b1": {ID: "b1", Type: "beta"},
		"b3": {ID: "b3", Type: "beta"},
	}, nil)

	started := make(chan string, 6)
	rpcClient := &fanoutRPCClient{call: func(
		ctx context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		started <- server.ID
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := NewRemoteServiceWithOptions(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
		WithFanoutConcurrency(2),
	)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan fanoutCallOutcome, 1)
	go func() {
		responses, err := svc.DoPublishRequest(
			ctx,
			route.NewRoute("", "events", "refresh"),
			nil,
			nil,
		)
		result <- fanoutCallOutcome{responses: responses, err: err}
	}()

	firstWave := []string{receiveFanoutValue(t, started), receiveFanoutValue(t, started)}
	sort.Strings(firstWave)
	assert.Equal(t, []string{"a1", "b1"}, firstWave)
	cancel()
	outcome := receiveFanoutValue(t, result)
	require.Error(t, outcome.err)
	assert.ErrorIs(t, outcome.err, context.Canceled)
	assert.Empty(t, outcome.responses)
	assert.Empty(t, started, "fork types must share one fanout concurrency limit")
}

func TestDoPublishRequestReturnsPartialResponsesWithDiscoveryErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	alpha := newSubscriberServer("alpha-representative", "alpha", "events", "refresh", true)
	beta := newSubscriberServer("beta-representative", "beta", "events", "refresh", true)
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{
		"beta":  beta,
		"alpha": alpha,
	})
	sd.EXPECT().GetServersByType("alpha").Return(map[string]*cluster.Server{
		"a": {ID: "a", Type: "alpha"},
	}, nil)
	discoveryErr := errors.New("beta discovery failed")
	sd.EXPECT().GetServersByType("beta").Return(nil, discoveryErr)

	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		return &protos.Response{Data: []byte(server.ID)}, nil
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
	)

	responses, err := svc.DoPublishRequest(
		context.Background(),
		route.NewRoute("", "events", "refresh"),
		nil,
		nil,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, discoveryErr)
	assert.ErrorContains(t, err, "server type beta")
	require.Len(t, responses, 1)
	assert.Equal(t, "a", string(responses[0].Data))
}

func TestDoPublishRequestReturnsResponsesInServerOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	alpha := newSubscriberServer("alpha-representative", "alpha", "events", "refresh", true)
	beta := newSubscriberServer("beta-representative", "beta", "events", "refresh", true)
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{
		"beta":  beta,
		"alpha": alpha,
	})
	sd.EXPECT().GetServersByType("alpha").Return(map[string]*cluster.Server{
		"c": {ID: "c", Type: "alpha"},
		"a": {ID: "a", Type: "alpha"},
	}, nil)
	sd.EXPECT().GetServersByType("beta").Return(map[string]*cluster.Server{
		"b": {ID: "b", Type: "beta"},
	}, nil)

	started := make(chan string, 3)
	finished := make(chan string, 3)
	release := map[string]chan struct{}{
		"a": make(chan struct{}),
		"b": make(chan struct{}),
		"c": make(chan struct{}),
	}
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		started <- server.ID
		<-release[server.ID]
		finished <- server.ID
		return &protos.Response{Data: []byte(server.ID)}, nil
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
	)
	result := make(chan fanoutCallOutcome, 1)
	go func() {
		responses, err := svc.DoPublishRequest(
			context.Background(),
			route.NewRoute("", "events", "refresh"),
			nil,
			nil,
		)
		result <- fanoutCallOutcome{responses: responses, err: err}
	}()

	for range release {
		receiveFanoutValue(t, started)
	}
	for _, serverID := range []string{"b", "c", "a"} {
		close(release[serverID])
		assert.Equal(t, serverID, receiveFanoutValue(t, finished))
	}
	outcome := receiveFanoutValue(t, result)
	require.NoError(t, outcome.err)
	require.Len(t, outcome.responses, 3)
	assert.Equal(t, "a", string(outcome.responses[0].Data))
	assert.Equal(t, "b", string(outcome.responses[1].Data))
	assert.Equal(t, "c", string(outcome.responses[2].Data))
}

func TestDoPublishRequestRoutesNonForkSubscriber(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	chat := newSubscriberServer("chat-representative", "chat", "events", "refresh", false)
	target := &cluster.Server{ID: "chat-1", Type: "chat"}
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{"chat": chat})
	sd.EXPECT().GetServersByType("chat").Return(map[string]*cluster.Server{"chat-1": target}, nil)

	rpcRouter := router.New()
	rpcRouter.SetServiceDiscovery(sd)
	called := make(chan string, 1)
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		route *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		called <- route.SvType + "/" + server.ID
		return &protos.Response{Data: []byte(server.ID)}, nil
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, rpcRouter, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
	)

	responses, err := svc.DoPublishRequest(
		context.Background(),
		route.NewRoute("", "events", "refresh"),
		nil,
		nil,
	)

	require.NoError(t, err)
	assert.Equal(t, "chat/chat-1", receiveFanoutValue(t, called))
	require.Len(t, responses, 1)
	assert.Equal(t, "chat-1", string(responses[0].Data))
}

func TestDoPublishRequestRoutesNonForkSubscribersInsideFanoutWorkers(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	alphaSubscriber := newSubscriberServer("alpha-representative", "alpha", "events", "refresh", false)
	betaSubscriber := newSubscriberServer("beta-representative", "beta", "events", "refresh", false)
	alphaTarget := &cluster.Server{ID: "alpha-1", Type: "alpha"}
	betaTarget := &cluster.Server{ID: "beta-1", Type: "beta"}
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{
		"alpha": alphaSubscriber,
		"beta":  betaSubscriber,
	})
	sd.EXPECT().GetServersByType("alpha").Return(map[string]*cluster.Server{"alpha-1": alphaTarget}, nil)
	sd.EXPECT().GetServersByType("beta").Return(map[string]*cluster.Server{"beta-1": betaTarget}, nil)

	routeStarted := make(chan string, 2)
	releaseAlphaRoute := make(chan struct{})
	rpcRouter := router.New()
	rpcRouter.SetServiceDiscovery(sd)
	rpcRouter.AddRoute("alpha", func(
		_ context.Context,
		_ *route.Route,
		_ []byte,
		servers map[string]*cluster.Server,
		_ session.Session,
	) (*cluster.Server, error) {
		routeStarted <- "alpha"
		<-releaseAlphaRoute
		return servers["alpha-1"], nil
	})
	rpcRouter.AddRoute("beta", func(
		_ context.Context,
		_ *route.Route,
		_ []byte,
		servers map[string]*cluster.Server,
		_ session.Session,
	) (*cluster.Server, error) {
		routeStarted <- "beta"
		return servers["beta-1"], nil
	})
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		server *cluster.Server,
	) (*protos.Response, error) {
		return &protos.Response{Data: []byte(server.ID)}, nil
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, rpcRouter, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
	)
	result := make(chan fanoutCallOutcome, 1)
	go func() {
		responses, err := svc.DoPublishRequest(
			context.Background(),
			route.NewRoute("", "events", "refresh"),
			nil,
			nil,
		)
		result <- fanoutCallOutcome{responses: responses, err: err}
	}()

	started := []string{receiveFanoutValue(t, routeStarted), receiveFanoutValue(t, routeStarted)}
	sort.Strings(started)
	assert.Equal(t, []string{"alpha", "beta"}, started)
	close(releaseAlphaRoute)
	outcome := receiveFanoutValue(t, result)
	require.NoError(t, outcome.err)
	require.Len(t, outcome.responses, 2)
	assert.Equal(t, "alpha-1", string(outcome.responses[0].Data))
	assert.Equal(t, "beta-1", string(outcome.responses[1].Data))
}

func TestDoPublishRequestWrapsNonForkErrorWithRoutedServerID(t *testing.T) {
	ctrl := gomock.NewController(t)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	chat := newSubscriberServer("chat-representative", "chat", "events", "refresh", false)
	target := &cluster.Server{ID: "chat-1", Type: "chat"}
	sd.EXPECT().GetServerTypes().Return(map[string]*cluster.Server{"chat": chat})
	sd.EXPECT().GetServersByType("chat").Return(map[string]*cluster.Server{"chat-1": target}, nil)

	rpcRouter := router.New()
	rpcRouter.SetServiceDiscovery(sd)
	callErr := errors.New("chat call failed")
	rpcClient := &fanoutRPCClient{call: func(
		_ context.Context,
		_ protos.RPCType,
		_ *route.Route,
		_ session.Session,
		_ *message.Message,
		_ *cluster.Server,
	) (*protos.Response, error) {
		return nil, callErr
	}}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, rpcRouter, nil,
		&cluster.Server{ID: "self", Type: "gateway"}, nil, nil, nil, nil,
	)

	responses, err := svc.DoPublishRequest(
		context.Background(),
		route.NewRoute("", "events", "refresh"),
		nil,
		nil,
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, callErr)
	assert.ErrorContains(t, err, "server chat-1")
	require.NotNil(t, responses)
	assert.Empty(t, responses)
}

func TestRegisterCalculatesForkFlagPerMethod(t *testing.T) {
	for i := 0; i < 20; i++ {
		server := &cluster.Server{Subscribe: make(map[string]*cluster.SubscribeItem)}
		svc := NewRemoteService(nil, nil, nil, nil, nil, nil, nil, server, nil, nil, nil, nil)
		err := svc.Register(&MyComp{}, []component.Option{
			component.WithSubscriber(),
			component.WithForkMethods("Remote1"),
		})
		require.NoError(t, err)

		for _, method := range []string{"Remote1", "Remote2", "RemoteErr", "RemoteRes"} {
			subscription := server.GetSubscribe("MyComp", method)
			require.NotNil(t, subscription)
			assert.Equal(t, method == "Remote1", subscription.Fork, method)
		}
	}
}
