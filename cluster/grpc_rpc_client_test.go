package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/interfaces"
	"github.com/alkaid/pitaya/v2/interfaces/mocks"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/protos"
	protosmocks "github.com/alkaid/pitaya/v2/protos/mocks"
	"github.com/alkaid/pitaya/v2/route"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

func getRPCClient(c config.GRPCClientConfig) (*GRPCClient, error) {
	sv := getServer()
	return NewGRPCClient(c, sv, []metrics.Reporter{}, nil, nil)
}

func TestNewGRPCClient(t *testing.T) {
	c := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
	g, err := getRPCClient(c)
	assert.NoError(t, err)
	assert.NotNil(t, g)
}

func TestNewGRPCClientRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.GRPCClientConfig)
		want   string
	}{
		{
			name:   "dial timeout",
			mutate: func(cfg *config.GRPCClientConfig) { cfg.DialTimeout = 0 },
			want:   "pitaya.cluster.rpc.client.grpc.dialtimeout",
		},
		{
			name:   "request timeout",
			mutate: func(cfg *config.GRPCClientConfig) { cfg.RequestTimeout = -time.Second },
			want:   "pitaya.cluster.rpc.client.grpc.requesttimeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
			test.mutate(&cfg)
			client, err := getRPCClient(cfg)
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestGRPCClientUnsupportedOperationsAndLifecycle(t *testing.T) {
	g, err := getRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc)
	require.NoError(t, err)
	require.NoError(t, g.Init())

	rt := route.NewRoute("room", "svc", "method")
	responses, err := g.Publish(
		context.Background(),
		protos.RPCType_Sys,
		rt,
		nil,
		&message.Message{Type: message.Notify},
		0,
	)
	assert.Nil(t, responses)
	assert.ErrorIs(t, err, constants.ErrNotImplemented)
	assert.ErrorIs(t, g.Fork(context.Background(), rt, nil, &message.Message{}), constants.ErrNotImplemented)
	assert.ErrorIs(t, g.Send("topic", []byte("payload")), constants.ErrNotImplemented)

	g.ModifyServer(NewServer("new", "room", false), NewServer("old", "room", false))
	g.AfterInit()
	g.BeforeShutdown()
	assert.NoError(t, g.Shutdown())
}

func TestGRPCClientShutdownClosesConnectionsAndAdmission(t *testing.T) {
	client, err := getRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc)
	require.NoError(t, err)
	require.NoError(t, client.Init())

	conn, err := grpc.NewClient(
		"passthrough:///127.0.0.1:1",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	target := NewServer("shutdown-target", "room", false)
	client.clientMap.Store(target.ID, &grpcClient{
		conn:      conn,
		cli:       protos.NewPitayaClient(conn),
		connected: true,
	})

	require.NoError(t, client.Shutdown())
	require.NoError(t, client.Shutdown())
	assert.False(t, client.IsRunning())
	assert.Equal(t, connectivity.Shutdown, conn.GetState())
	_, found := client.clientMap.Load(target.ID)
	assert.False(t, found)

	_, err = client.Call(
		context.Background(),
		protos.RPCType_Sys,
		route.NewRoute("room", "svc", "method"),
		nil,
		&message.Message{Type: message.Notify},
		target,
	)
	assert.ErrorIs(t, err, constants.ErrRPCClientNotInitialized)
	client.AddServer(&Server{
		ID:   "late",
		Type: "room",
		Metadata: map[string]string{
			constants.GRPCHostKey: "127.0.0.1",
			constants.GRPCPortKey: "1",
		},
	})
	_, found = client.clientMap.Load("late")
	assert.False(t, found)
}

func TestGRPCClientClosedAdmissionRejectsEveryOperation(t *testing.T) {
	client, err := getRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc)
	require.NoError(t, err)
	require.NoError(t, client.Shutdown())

	rt := route.NewRoute("room", "svc", "method")
	responses, err := client.Publish(context.Background(), protos.RPCType_Sys, rt, nil, &message.Message{}, 0)
	assert.Nil(t, responses)
	assert.ErrorIs(t, err, constants.ErrRPCClientNotInitialized)
	assert.ErrorIs(t, client.Fork(context.Background(), rt, nil, &message.Message{}), constants.ErrRPCClientNotInitialized)
	assert.ErrorIs(t, client.Send("topic", nil), constants.ErrRPCClientNotInitialized)
	assert.ErrorIs(t, client.BroadcastSessionBind("uid"), constants.ErrRPCClientNotInitialized)
	assert.ErrorIs(t, client.SendKick("uid", "room", &protos.KickMsg{}), constants.ErrRPCClientNotInitialized)
	assert.ErrorIs(t, client.SendPush("uid", nil, &protos.Push{}), constants.ErrRPCClientNotInitialized)
	assert.ErrorIs(t, client.Init(), constants.ErrRPCClientNotInitialized)

	client.RemoveServer(NewServer("late", "room", false))
	_, found := client.clientMap.Load("late")
	assert.False(t, found)
}

func TestGRPCClientBeginOperationNormalizesAndRejectsContext(t *testing.T) {
	client, err := getRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc)
	require.NoError(t, err)

	ctx, done, err := client.beginOperation(nil)
	require.NoError(t, err)
	require.NotNil(t, ctx)
	done()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, done, err = client.beginOperation(canceled)
	assert.Nil(t, ctx)
	assert.Nil(t, done)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGRPCLowLevelClientReportsMissingStub(t *testing.T) {
	client := &grpcClient{connected: true}

	got, err := client.client()
	assert.Nil(t, got)
	assert.ErrorIs(t, err, constants.ErrNoConnectionToServer)
	assert.ErrorIs(t, client.pushToUser(context.Background(), &protos.Push{}), constants.ErrNoConnectionToServer)
	_, err = client.call(context.Background(), &protos.Request{})
	assert.ErrorIs(t, err, constants.ErrNoConnectionToServer)
	assert.ErrorIs(t, client.sessionBindRemote(context.Background(), &protos.BindMsg{}), constants.ErrNoConnectionToServer)
	assert.ErrorIs(t, client.sendKick(context.Background(), &protos.KickMsg{}), constants.ErrNoConnectionToServer)
}

func TestGRPCClientShutdownCancelsAndJoinsAdmittedCall(t *testing.T) {
	client, err := getRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc)
	require.NoError(t, err)
	require.NoError(t, client.Init())

	ctrl := gomock.NewController(t)
	mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)
	target := NewServer("blocking-target", "room", false)
	callStarted := make(chan struct{})
	mockPitayaClient.EXPECT().Call(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ *protos.Request, _ ...grpc.CallOption) (*protos.Response, error) {
			close(callStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	client.clientMap.Store(target.ID, &grpcClient{cli: mockPitayaClient, connected: true})

	callDone := make(chan error, 1)
	go func() {
		_, callErr := client.Call(
			context.Background(),
			protos.RPCType_Sys,
			route.NewRoute("room", "svc", "method"),
			nil,
			&message.Message{Type: message.Notify},
			target,
		)
		callDone <- callErr
	}()
	select {
	case <-callStarted:
	case <-time.After(time.Second):
		t.Fatal("gRPC call was not admitted")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- client.Shutdown() }()
	select {
	case callErr := <-callDone:
		assert.ErrorIs(t, callErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel admitted gRPC call")
	}
	select {
	case shutdownErr := <-shutdownDone:
		require.NoError(t, shutdownErr)
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join admitted gRPC call")
	}
}

func TestCall(t *testing.T) {
	c := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
	g, err := getRPCClient(c)
	assert.NoError(t, err)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)
	g.clientMap.Store(g.server.ID, &grpcClient{
		cli:       mockPitayaClient,
		connected: true,
	})

	uid := "someuid"
	frontendID := "frontend-forwarded"
	msg := &message.Message{
		Type:  0,
		ID:    0,
		Route: "sv.svc.meth",
		Data:  []byte{0x01},
		Err:   false,
	}

	ctx := context.Background()
	rpcType := protos.RPCType_Sys
	r := route.NewRoute("sv", "svc", "meth")

	sess := sessionmocks.NewMockSession(ctrl)
	sess.EXPECT().GetFrontendID().Return(frontendID).Times(2)
	sess.EXPECT().GetFrontendSessionID().Return(int64(1)).Times(2)
	sess.EXPECT().UID().Return(uid).Times(2)
	sess.EXPECT().GetDataEncoded().Return(nil).Times(2)
	sess.EXPECT().GetBackends().Return(nil).Times(2)
	sess.EXPECT().RemoteIPText().Return("").Times(2)
	sess.EXPECT().SetRequestInFlight(gomock.Any(), gomock.Any(), gomock.Any()).Times(2)

	expected, err := buildRequest(ctx, rpcType, r.String(), sess, msg, g.server)
	assert.NoError(t, err)

	mockPitayaClient.EXPECT().Call(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, in *protos.Request, opts ...grpc.CallOption) (*protos.Response, error) {
		assert.Equal(t, frontendID, in.FrontendID)
		assert.Equal(t, expected.FrontendID, in.FrontendID)
		assert.Equal(t, expected.Type, in.Type)
		assert.Equal(t, expected.Msg, in.Msg)
		return &protos.Response{Data: []byte{0x01}}, nil
	})

	res, err := g.Call(ctx, rpcType, r, sess, msg, g.server)

	assert.NoError(t, err)
	assert.NotNil(t, res)
}

func TestGRPCClientCallOwnsTargetFencingMetadata(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
	client, err := getRPCClient(cfg)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)
	target := NewServer("grpc-fenced", "room", false)
	target.SetOwnerEpoch(42)
	client.clientMap.Store(target.ID, &grpcClient{cli: mockPitayaClient, connected: true})

	mockPitayaClient.EXPECT().Call(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, request *protos.Request, _ ...grpc.CallOption) (*protos.Response, error) {
			requestCtx, decodeErr := pcontext.Decode(request.Metadata)
			require.NoError(t, decodeErr)
			assert.Equal(t, target.ID, pcontext.GetFromPropagateCtx(requestCtx, constants.OwnerServerIDKey))
			assert.Equal(t, "42", pcontext.GetFromPropagateCtx(requestCtx, constants.OwnerEpochKey))
			return &protos.Response{}, nil
		},
	)

	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "stale-target",
		constants.OwnerEpochKey, "1",
	)
	_, err = client.Call(
		ctx,
		protos.RPCType_User,
		route.NewRoute(target.Type, "svc", "notify"),
		nil,
		&message.Message{Type: message.Notify},
		target,
	)
	require.NoError(t, err)

	target.InvalidateOwnerEpoch()
	_, err = client.Call(
		context.Background(),
		protos.RPCType_User,
		route.NewRoute(target.Type, "svc", "notify"),
		nil,
		&message.Message{Type: message.Notify},
		target,
	)
	require.ErrorIs(t, err, constants.ErrStaleOwnerEpoch)
}

func TestBroadcastSessionBind(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBindingStorage := mocks.NewMockBindingStorage(ctrl)
	mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)
	tables := []struct {
		name           string
		bindingStorage interfaces.BindingStorage
		err            error
	}{{"shouldrun", mockBindingStorage, nil}, {"shoulderror", nil, constants.ErrNoBindingStorageModule}}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			c := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
			g, err := getRPCClient(c)
			assert.NoError(t, err)
			uid := "someuid"
			//mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)

			if table.bindingStorage != nil {
				g.clientMap.Store(g.server.ID, &grpcClient{connected: true, cli: mockPitayaClient})

				g.bindingStorage = mockBindingStorage
				mockBindingStorage.EXPECT().GetUserFrontendID(uid, gomock.Any()).DoAndReturn(func(u, svType string) (string, error) {
					assert.Equal(t, uid, u)
					assert.Equal(t, g.server.Type, svType)

					return g.server.ID, nil
				})

				mockPitayaClient.EXPECT().SessionBindRemote(gomock.Any(), gomock.Any()).Do(func(ctx context.Context, msg *protos.BindMsg, opts ...grpc.CallOption) {
					assert.Equal(t, uid, msg.Uid, g.server.ID, msg.Fid)
				})
			}

			err = g.BroadcastSessionBind(uid)
			if table.err != nil {
				assert.EqualError(t, err, table.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSendKick(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBindingStorage := mocks.NewMockBindingStorage(ctrl)
	mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)
	tables := []struct {
		name           string
		userID         string
		bindingStorage interfaces.BindingStorage
		sv             *Server
		err            error
	}{
		{"bindingstorage", "uid", mockBindingStorage, &Server{
			Type:     "tp",
			Frontend: true}, nil,
		},
		{"nobindingstorage", "uid", nil, &Server{
			Type:     "tp",
			Frontend: true,
		}, constants.ErrNoBindingStorageModule},
		{"nobindingstorage", "", mockBindingStorage, &Server{
			Type:     "tp",
			Frontend: true,
		}, nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			c := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
			g, err := getRPCClient(c)
			assert.NoError(t, err)

			if table.bindingStorage != nil {
				g.clientMap.Store(table.sv.ID, &grpcClient{connected: true, cli: mockPitayaClient})
				g.bindingStorage = table.bindingStorage
				mockBindingStorage.EXPECT().GetUserFrontendID(table.userID, gomock.Any()).DoAndReturn(func(u, svType string) (string, error) {
					assert.Equal(t, table.userID, u)
					assert.Equal(t, table.sv.Type, svType)
					return table.sv.ID, nil
				})

				mockPitayaClient.EXPECT().KickUser(gomock.Any(), gomock.Any()).Do(func(ctx context.Context, msg *protos.KickMsg, opts ...grpc.CallOption) {
					assert.Equal(t, table.userID, msg.UserId)
				})
			}

			kick := &protos.KickMsg{
				UserId: table.userID,
			}

			err = g.SendKick(table.userID, table.sv.Type, kick)
			if table.err != nil {
				assert.Equal(t, err, table.err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSendPush(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBindingStorage := mocks.NewMockBindingStorage(ctrl)
	mockPitayaClient := protosmocks.NewMockPitayaClient(ctrl)
	tables := []struct {
		name           string
		bindingStorage interfaces.BindingStorage
		sv             *Server
		err            error
	}{
		{"bindingstorage-no-fid", mockBindingStorage, &Server{
			Type:     "tp",
			Frontend: true,
		}, nil},
		{"nobindingstorage-no-fid", nil, &Server{
			Type:     "tp",
			Frontend: true,
		}, constants.ErrNoBindingStorageModule},
		{"nobindingstorage-fid", nil, &Server{
			ID:       "someID",
			Type:     "tp",
			Frontend: true,
		}, nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			g, err := getRPCClient(config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc)
			assert.NoError(t, err)
			uid := "someuid"

			if table.bindingStorage != nil && table.sv.ID == "" {
				g.clientMap.Store(table.sv.ID, &grpcClient{connected: true, cli: mockPitayaClient})
				g.bindingStorage = table.bindingStorage
				mockBindingStorage.EXPECT().GetUserFrontendID(uid, gomock.Any()).DoAndReturn(func(u, svType string) (string, error) {
					assert.Equal(t, uid, u)
					assert.Equal(t, table.sv.Type, svType)
					return table.sv.ID, nil
				})

				mockPitayaClient.EXPECT().PushToUser(gomock.Any(), gomock.Any()).Do(func(ctx context.Context, msg *protos.Push, opts ...grpc.CallOption) {
					assert.Equal(t, uid, msg.Uid)
					assert.Equal(t, msg.Route, "sv.svc.mth")
					assert.Equal(t, msg.Data, []byte{0x01})
				})
			} else if table.bindingStorage == nil && table.sv.ID != "" {
				g.clientMap.Store(table.sv.ID, &grpcClient{connected: true, cli: mockPitayaClient})
				mockPitayaClient.EXPECT().PushToUser(gomock.Any(), gomock.Any()).Do(func(ctx context.Context, msg *protos.Push, opts ...grpc.CallOption) {
					assert.Equal(t, uid, msg.Uid)
					assert.Equal(t, msg.Route, "sv.svc.mth")
					assert.Equal(t, msg.Data, []byte{0x01})
				})
			}

			push := &protos.Push{
				Route: "sv.svc.mth",
				Uid:   uid,
				Data:  []byte{0x01},
			}

			err = g.SendPush(uid, table.sv, push)

			if table.err != nil {
				assert.EqualError(t, err, table.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAddServer(t *testing.T) {
	t.Run("try-connect", func(t *testing.T) {
		// listen
		clientConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc

		serverConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
		serverConfig.Port = helpers.GetFreePort(t)
		server := &Server{
			ID:   "someid",
			Type: "sometype",
			Metadata: map[string]string{
				constants.GRPCHostKey: "localhost",
				constants.GRPCPortKey: fmt.Sprintf("%d", serverConfig.Port),
			},
			Frontend: false,
		}
		gs, err := NewGRPCServer(serverConfig, server, []metrics.Reporter{})
		assert.NoError(t, err)

		gs.SetPitayaServer(&protos.UnimplementedPitayaServer{})

		err = gs.Init()
		assert.NoError(t, err)
		// --- should connect to the server and add it to the client map
		g, err := getRPCClient(clientConfig)
		assert.NoError(t, err)
		g.AddServer(server)

		sv, ok := g.clientMap.Load(server.ID)
		assert.NotNil(t, sv)
		assert.True(t, ok)
		cli := sv.(*grpcClient)
		assert.True(t, cli.connected)
		assert.NotNil(t, cli.cli)
	})

	t.Run("lazy", func(t *testing.T) {
		// listen
		clientConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc
		clientConfig.LazyConnection = true

		serverConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
		serverConfig.Port = helpers.GetFreePort(t)

		server := &Server{
			ID:   "someid",
			Type: "sometype",
			Metadata: map[string]string{
				constants.GRPCHostKey: "localhost",
				constants.GRPCPortKey: fmt.Sprintf("%d", serverConfig.Port),
			},
			Frontend: false,
		}
		gs, err := NewGRPCServer(serverConfig, server, []metrics.Reporter{})
		assert.NoError(t, err)

		gs.SetPitayaServer(&protos.UnimplementedPitayaServer{})

		err = gs.Init()
		assert.NoError(t, err)
		g, err := getRPCClient(clientConfig)
		assert.NoError(t, err)
		// --- should not connect to the server and add it to the client map
		g.AddServer(server)

		sv, ok := g.clientMap.Load(server.ID)
		assert.NotNil(t, sv)
		assert.True(t, ok)
		cli := sv.(*grpcClient)
		assert.False(t, cli.connected)
		assert.Nil(t, cli.cli)
	})
}

func TestGetServerHost(t *testing.T) {
	t.Parallel()

	var (
		host         = "host"
		externalHost = "externalHost"
		region       = "region"
	)

	tables := map[string]struct {
		metadata        map[string]string
		clientRegion    string
		expectedHost    string
		expectedPortKey string
	}{
		"test_has_no_region_and_no_external_host": {
			metadata: map[string]string{
				constants.GRPCHostKey: host,
			},
			expectedHost:    host,
			expectedPortKey: constants.GRPCPortKey,
		},
		"test_has_no_region_and_external_host": {
			metadata: map[string]string{
				constants.GRPCExternalHostKey: externalHost,
			},
			expectedHost:    externalHost,
			expectedPortKey: constants.GRPCExternalPortKey,
		},
		"test_has_region_and_same_region": {
			metadata: map[string]string{
				constants.GRPCHostKey: host,
				constants.RegionKey:   region,
			},
			clientRegion:    region,
			expectedHost:    host,
			expectedPortKey: constants.GRPCPortKey,
		},
		"test_has_region_and_other_region": {
			metadata: map[string]string{
				constants.GRPCExternalHostKey: externalHost,
				constants.RegionKey:           region,
			},
			clientRegion:    "other-region",
			expectedHost:    externalHost,
			expectedPortKey: constants.GRPCExternalPortKey,
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.Info
			config.Region = table.clientRegion
			infoRetriever := NewInfoRetriever(config)
			gs := &GRPCClient{infoRetriever: infoRetriever}

			host, portKey := gs.getServerHost(&Server{
				Metadata: table.metadata,
			})

			assert.Equal(t, table.expectedHost, host)
			assert.Equal(t, table.expectedPortKey, portKey)
		})
	}
}

func TestRemoveServer(t *testing.T) {
	clientConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Client.Grpc

	serverConfig := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	serverConfig.Port = helpers.GetFreePort(t)

	server := &Server{
		ID:   "someid",
		Type: "sometype",
		Metadata: map[string]string{
			constants.GRPCHostKey: "localhost",
			constants.GRPCPortKey: fmt.Sprintf("%d", serverConfig.Port),
		},
		Frontend: false,
	}
	gs, err := NewGRPCServer(serverConfig, server, []metrics.Reporter{})
	assert.NoError(t, err)
	gs.SetPitayaServer(&protos.UnimplementedPitayaServer{})
	err = gs.Init()
	assert.NoError(t, err)

	gc, err := NewGRPCClient(clientConfig, server, []metrics.Reporter{}, nil, nil)
	assert.NoError(t, err)
	gc.AddServer(server)

	sv, ok := gc.clientMap.Load(server.ID)
	assert.NotNil(t, sv)
	assert.True(t, ok)

	gc.RemoveServer(server)

	sv, ok = gc.clientMap.Load(server.ID)
	assert.Nil(t, sv)
	assert.False(t, ok)
}
