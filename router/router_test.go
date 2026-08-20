package router

import (
	"context"
	"errors"
	"testing"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type statefulRoutingTestDiscovery struct {
	cluster.ServiceDiscovery
	servers map[string]*cluster.Server
	hash    func(string, string) (string, error)
	resolve func(context.Context, string, string, string) (*cluster.Server, cluster.StatefulOwnerFence, error)
}

func (d *statefulRoutingTestDiscovery) GetServersByType(string) (map[string]*cluster.Server, error) {
	return d.servers, nil
}

func (d *statefulRoutingTestDiscovery) GetConsistentHashNode(serverType, routingKey string) (string, error) {
	if d.hash == nil {
		return "", errors.New("unexpected consistent-hash lookup")
	}
	return d.hash(serverType, routingKey)
}

func (d *statefulRoutingTestDiscovery) ResolveStatefulOwner(
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

func (d *statefulRoutingTestDiscovery) ValidateStatefulOwner(
	context.Context,
	string,
	string,
	cluster.StatefulOwnerFence,
) error {
	return errors.New("unexpected stateful owner validation")
}

var (
	serverID   = "id"
	serverType = "serverType"
	frontend   = true
	server     = cluster.NewServer(serverID, serverType, frontend)
	servers    = map[string]*cluster.Server{
		serverID: server,
	}

	routingFunction = func(
		ctx context.Context,
		route *route.Route,
		payload []byte,
		servers map[string]*cluster.Server,
		session session.Session,
	) (*cluster.Server, error) {
		return server, nil
	}
)

var routerTables = map[string]struct {
	server     *cluster.Server
	serverType string
	rpcType    protos.RPCType
	err        error
}{
	"test_server_has_route_func":   {server, serverType, protos.RPCType_Sys, nil},
	"test_server_use_default_func": {server, "notRegisteredType", protos.RPCType_Sys, nil},
	"test_user_use_default_func":   {server, serverType, protos.RPCType_User, nil},
	"test_error_on_service_disc":   {nil, serverType, protos.RPCType_Sys, errors.New("sd error")},
}

var addRouteRouterTables = map[string]struct {
	serverType string
}{
	"test_overrige_server_type": {serverType},
	"test_new_server_type":      {"notRegisteredType"},
}

func TestNew(t *testing.T) {
	t.Parallel()
	router := New()
	assert.NotNil(t, router)
}

func TestDefaultRoute(t *testing.T) {
	t.Parallel()

	router := New()

	retServer, err := router.defaultRoute(context.Background(), serverType, servers, nil)
	assert.NoError(t, err)
	assert.Equal(t, server, retServer)
}

func TestStatefulOwnerAssignmentOnlyAppliesToDefaultConsistentHashRoute(t *testing.T) {
	t.Run("default consistent hash", func(t *testing.T) {
		target := cluster.NewServer("stateful-1", "stateful", false)
		routingIdentity, err := cluster.StatefulOwnerRoutingKey("user-1", "", 0)
		require.NoError(t, err)
		assignmentKey, err := cluster.StatefulOwnerAssignmentKey(target.Type, routingIdentity)
		require.NoError(t, err)
		wantFence := cluster.StatefulOwnerFence{
			AssignmentKey: assignmentKey,
			Epoch:         73,
			ServerID:      target.ID,
		}
		var resolveCalls int
		discovery := &statefulRoutingTestDiscovery{
			servers: map[string]*cluster.Server{target.ID: target},
			hash: func(serverType, routingKey string) (string, error) {
				assert.Equal(t, target.Type, serverType)
				assert.Equal(t, "user-1", routingKey)
				return target.ID, nil
			},
			resolve: func(
				_ context.Context,
				serverType string,
				routingKey string,
				candidateServerID string,
			) (*cluster.Server, cluster.StatefulOwnerFence, error) {
				resolveCalls++
				assert.Equal(t, target.Type, serverType)
				assert.Equal(t, routingIdentity, routingKey)
				assert.Equal(t, target.ID, candidateServerID)
				return target, wantFence, nil
			},
		}
		r := New()
		r.SetServiceDiscovery(discovery)
		sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")

		got, err := r.defaultRoute(context.Background(), target.Type, discovery.servers, sess)
		require.NoError(t, err)
		assert.NotSame(t, target, got, "the assignment belongs to a request-scoped server snapshot")
		fence, ok := got.StatefulOwnerFenceSnapshot()
		require.True(t, ok)
		assert.Equal(t, wantFence, fence)
		assert.Equal(t, 1, resolveCalls)
		_, originalWasMutated := target.StatefulOwnerFenceSnapshot()
		assert.False(t, originalWasMutated)
	})

	t.Run("session stickiness", func(t *testing.T) {
		target := cluster.NewServer("sticky-1", "stateful", false)
		target.SessionStickiness = true
		discovery := &statefulRoutingTestDiscovery{
			servers: map[string]*cluster.Server{target.ID: target},
			resolve: func(context.Context, string, string, string) (*cluster.Server, cluster.StatefulOwnerFence, error) {
				t.Fatal("a bound backend must remain a direct target")
				return nil, cluster.StatefulOwnerFence{}, nil
			},
		}
		r := New()
		r.SetServiceDiscovery(discovery)
		sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
		sess.SetBackendID(target.Type, target.ID)

		got, err := r.defaultRoute(context.Background(), target.Type, discovery.servers, sess)
		require.NoError(t, err)
		assert.Same(t, target, got)
		_, assigned := got.StatefulOwnerFenceSnapshot()
		assert.False(t, assigned)
	})

	t.Run("custom route", func(t *testing.T) {
		target := cluster.NewServer("custom-1", "stateful", false)
		discovery := &statefulRoutingTestDiscovery{
			servers: map[string]*cluster.Server{target.ID: target},
			resolve: func(context.Context, string, string, string) (*cluster.Server, cluster.StatefulOwnerFence, error) {
				t.Fatal("a custom route result must remain a direct target")
				return nil, cluster.StatefulOwnerFence{}, nil
			},
		}
		r := New()
		r.SetServiceDiscovery(discovery)
		r.AddRoute(target.Type, func(
			context.Context,
			*route.Route,
			[]byte,
			map[string]*cluster.Server,
			session.Session,
		) (*cluster.Server, error) {
			return target, nil
		})
		sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")

		got, err := r.Route(
			context.Background(),
			protos.RPCType_User,
			target.Type,
			route.NewRoute(target.Type, "player", "move"),
			&message.Message{},
			sess,
		)
		require.NoError(t, err)
		assert.Same(t, target, got)
		_, assigned := got.StatefulOwnerFenceSnapshot()
		assert.False(t, assigned)
	})
}

func TestDefaultRouteDoesNotRandomlyFallbackAfterConsistentHashFailure(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	discovery := mocks.NewMockServiceDiscovery(ctrl)
	sess := sessionmocks.NewMockSession(ctrl)
	target := cluster.NewServer("stateful-1", "stateful", false)
	hashErr := errors.New("hash ring unavailable")

	sess.EXPECT().UID().Return("uid-1").AnyTimes()
	sess.EXPECT().GetFrontendID().Return("").AnyTimes()
	sess.EXPECT().GetFrontendSessionID().Return(int64(0)).AnyTimes()
	discovery.EXPECT().GetConsistentHashNode("stateful", "uid-1").Return("", hashErr)

	r := New()
	r.SetServiceDiscovery(discovery)
	got, err := r.defaultRoute(context.Background(), "stateful", map[string]*cluster.Server{target.ID: target}, sess)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, hashErr)
}

func TestDefaultRouteRejectsStatefulSessionWithoutStableKey(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	sess := sessionmocks.NewMockSession(ctrl)
	target := cluster.NewServer("stateful-1", "stateful", false)

	sess.EXPECT().UID().Return("").AnyTimes()
	sess.EXPECT().GetFrontendID().Return("").AnyTimes()
	sess.EXPECT().GetFrontendSessionID().Return(int64(0)).AnyTimes()

	got, err := New().defaultRoute(context.Background(), "stateful", map[string]*cluster.Server{target.ID: target}, sess)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, constants.ErrStatefulRouteKeyNotFound)
}

func TestDefaultRouteRejectsEmptyServerSet(t *testing.T) {
	t.Parallel()
	got, err := New().defaultRoute(context.Background(), "empty", nil, nil)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, constants.ErrNoServersAvailableOfType)
}

func TestRoute(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	route := route.NewRoute(serverType, "service", "method")

	for name, table := range routerTables {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockServiceDiscovery := mocks.NewMockServiceDiscovery(ctrl)
			mockServiceDiscovery.EXPECT().
				GetServersByType(table.serverType).
				Return(servers, table.err)

			router := New()
			router.AddRoute(serverType, routingFunction)
			router.SetServiceDiscovery(mockServiceDiscovery)

			retServer, err := router.Route(ctx, table.rpcType, table.serverType, route, &message.Message{
				Data: []byte{0x01},
			}, nil)
			assert.Equal(t, table.server, retServer)
			if table.err != nil {
				assert.ErrorIs(t, err, table.err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAddRoute(t *testing.T) {
	t.Parallel()

	for name, table := range addRouteRouterTables {
		t.Run(name, func(t *testing.T) {
			router := New()
			router.AddRoute(table.serverType, routingFunction)

			assert.NotNil(t, router.routesMap[table.serverType])
			assert.Nil(t, router.routesMap["anotherServerType"])
		})
	}
}
