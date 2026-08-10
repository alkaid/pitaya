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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2"
	"github.com/alkaid/pitaya/v2/acceptor"
	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/examples/testing/protos"
	"github.com/alkaid/pitaya/v2/groups"
	"github.com/alkaid/pitaya/v2/modules"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/alkaid/pitaya/v2/serialize/json"
	"github.com/alkaid/pitaya/v2/serialize/protobuf"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/viper"
	"github.com/google/uuid"
)

// TestSvc service for e2e tests
type TestSvc struct {
	component.Base
	app               pitaya.Pitaya
	sessionPool       session.SessionPool
	stressCloseEvents sync.Map
}

// TestRemoteSvc remote service for e2e tests
type TestRemoteSvc struct {
	component.Base
}

// TestRPCRequest for e2e tests
type TestRPCRequest struct {
	Route string `json:"route"`
	Data  string `json:"data"`
}

// TestSendToUsers for e2e tests
type TestSendToUsers struct {
	UIDs []string `json:"uids"`
	Msg  string   `json:"msg"`
}

// RPCTestRawPtrReturnsPtr remote for e2e tests
func (tr *TestRemoteSvc) RPCTestRawPtrReturnsPtr(ctx context.Context, data *test.TestRequest) (*test.TestResponse, error) {
	return &test.TestResponse{
		Code: 200,
		Msg:  fmt.Sprintf("got %s", data.GetMsg()),
	}, nil
}

// RPCTestPtrReturnsPtr remote for e2e tests
func (tr *TestRemoteSvc) RPCTestPtrReturnsPtr(ctx context.Context, req *test.TestRequest) (*test.TestResponse, error) {
	return &test.TestResponse{
		Code: 200,
		Msg:  fmt.Sprintf("got %s", req.Msg),
	}, nil
}

// RPCTestReturnsError remote for e2e tests
func (tr *TestRemoteSvc) RPCTestReturnsError(ctx context.Context, data *test.TestRequest) (*test.TestResponse, error) {
	err := errors.New("test error")
	return nil, apierrors.InternalServer("PIT-433", err.Error(), "").
		WithCause(err).
		WithMetadata(map[string]string{"some": "meta"})
}

// RPCTestNoArgs remote for e2e tests
func (tr *TestRemoteSvc) RPCTestNoArgs(ctx context.Context) (*test.TestResponse, error) {
	return &test.TestResponse{
		Code: 200,
		Msg:  "got nothing",
	}, nil
}

// Init inits testsvc
func (t *TestSvc) Init() {
	err := t.app.GroupCreate(context.Background(), "g1")
	if err != nil {
		panic(err)
	}
	t.app.OnSessionClose(func(s session.Session, _ map[string]string, reason session.CloseReason) {
		t.notifyStressClose(s.ID(), reason)
	})
}

func (t *TestSvc) notifyStressClose(sessionID int64, reason session.CloseReason) {
	event, tracked := t.stressCloseEvents.LoadAndDelete(sessionID)
	if !tracked {
		return
	}
	closeEvent, ok := event.(chan session.CloseReason)
	if !ok {
		return
	}
	select {
	case closeEvent <- reason:
	default:
	}
}

// TestRequestKickUser handler for e2e tests
func (t *TestSvc) TestRequestKickUser(ctx context.Context, userID []byte) (*test.TestResponse, error) {
	s := t.sessionPool.GetSessionByUID(string(userID))
	if s == nil {
		return nil, apierrors.NotFound("PIT-404", constants.ErrSessionNotFound.Error(), "").
			WithCause(constants.ErrSessionNotFound)
	}
	err := s.Kick(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &test.TestResponse{
		Code: 200,
		Msg:  "ok",
	}, nil
}

// TestRequestKickMe handler for e2e tests
func (t *TestSvc) TestRequestKickMe(ctx context.Context) (*test.TestResponse, error) {
	s := t.app.GetSessionFromCtx(ctx)
	if s == nil {
		return nil, apierrors.NotFound("PIT-404", constants.ErrSessionNotFound.Error(), "").
			WithCause(constants.ErrSessionNotFound)
	}
	err := s.Kick(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &test.TestResponse{
		Code: 200,
		Msg:  "ok",
	}, nil
}

// TestRequestOnlySessionReturnsPtr handler for e2e tests
func (t *TestSvc) TestRequestOnlySessionReturnsPtr(ctx context.Context) (*test.TestResponse, error) {
	return &test.TestResponse{
		Code: 200,
		Msg:  "hello",
	}, nil
}

// TestRequestOnlySessionReturnsPtrNil handler for e2e tests
func (t *TestSvc) TestRequestOnlySessionReturnsPtrNil(ctx context.Context) (*test.TestResponse, error) {
	return nil, nil
}

// TestRequestReturnsPtr handler for e2e tests
func (t *TestSvc) TestRequestReturnsPtr(ctx context.Context, in *test.TestRequest) (*test.TestResponse, error) {
	return &test.TestResponse{
		Code: 200,
		Msg:  in.Msg,
	}, nil
}

// TestRequestOnlySessionReturnsRawNil handler for e2e tests
func (t *TestSvc) TestRequestOnlySessionReturnsRawNil(ctx context.Context) ([]byte, error) {
	return nil, nil
}

// TestRequestReturnsRaw handler for e2e tests
func (t *TestSvc) TestRequestReturnsRaw(ctx context.Context, in *test.TestRequest) ([]byte, error) {
	return []byte(in.Msg), nil
}

// TestRequestReceiveReturnsRaw handler for e2e tests
func (t *TestSvc) TestRequestReceiveReturnsRaw(ctx context.Context, in []byte) ([]byte, error) {
	return in, nil
}

// TestRequestReturnsError handler for e2e tests
func (t *TestSvc) TestRequestReturnsError(ctx context.Context, in []byte) ([]byte, error) {
	err := errors.New("somerror")
	return nil, apierrors.InternalServer("PIT-555", err.Error(), "").WithCause(err)
}

// TestBind handler for e2e tests
func (t *TestSvc) TestBind(ctx context.Context) ([]byte, error) {
	uid := uuid.New().String()
	s := t.app.GetSessionFromCtx(ctx)
	err := s.Bind(ctx, uid, nil, nil)
	if err != nil {
		return nil, apierrors.InternalServer("PIT-444", err.Error(), "").WithCause(err)
	}
	err = t.app.GroupAddMember(ctx, "g1", s.UID())
	if err != nil {
		return nil, apierrors.InternalServer("PIT-441", err.Error(), "").WithCause(err)
	}
	return []byte("ack"), nil
}

// TestBindID handler for e2e tests
func (t *TestSvc) TestBindID(ctx context.Context, byteUID []byte) ([]byte, error) {
	s := t.app.GetSessionFromCtx(ctx)
	err := s.Bind(ctx, string(byteUID), nil, nil)
	if err != nil {
		return nil, apierrors.InternalServer("PIT-444", err.Error(), "").WithCause(err)
	}
	err = t.app.GroupAddMember(ctx, "g1", s.UID())
	if err != nil {
		return nil, apierrors.InternalServer("PIT-441", err.Error(), "").WithCause(err)
	}
	return []byte("ack"), nil
}

// TestStressBindID binds without group side effects so same-UID replacement is isolated.
func (t *TestSvc) TestStressBindID(ctx context.Context, byteUID []byte) ([]byte, error) {
	s := t.app.GetSessionFromCtx(ctx)
	closeEvent := make(chan session.CloseReason, 1)
	if _, loaded := t.stressCloseEvents.LoadOrStore(s.ID(), closeEvent); loaded {
		err := fmt.Errorf("session %d already has a stress close tracker", s.ID())
		return nil, apierrors.InternalServer("PIT-448", err.Error(), "").WithCause(err)
	}
	bound := false
	defer func() {
		if !bound {
			t.stressCloseEvents.Delete(s.ID())
		}
	}()
	if err := s.Bind(ctx, string(byteUID), nil, nil); err != nil {
		return nil, apierrors.InternalServer("PIT-444", err.Error(), "").WithCause(err)
	}
	bound = true
	return []byte("ack"), nil
}

// TestStressRebindID requires and replaces a distinct current owner.
func (t *TestSvc) TestStressRebindID(ctx context.Context, byteUID []byte) ([]byte, error) {
	s := t.app.GetSessionFromCtx(ctx)
	uid := string(byteUID)
	previous := t.sessionPool.GetSessionByUID(uid)
	if previous == nil || previous.ID() == s.ID() {
		err := fmt.Errorf("uid %q has no distinct owner to replace", uid)
		return nil, apierrors.InternalServer("PIT-446", err.Error(), "").WithCause(err)
	}
	previousID := previous.ID()
	closeEventValue, tracked := t.stressCloseEvents.Load(previousID)
	closeEvent, eventOK := closeEventValue.(chan session.CloseReason)
	if !tracked || !eventOK {
		err := fmt.Errorf("uid %q previous session %d has no close tracker", uid, previousID)
		return nil, apierrors.InternalServer("PIT-448", err.Error(), "").WithCause(err)
	}
	if err := s.Bind(ctx, uid, nil, nil); err != nil {
		return nil, apierrors.InternalServer("PIT-444", err.Error(), "").WithCause(err)
	}
	// The sys bind callback writes the kick before committing Close. The peer's
	// EOF can therefore win and record Normal; both outcomes must still be
	// observed before this rebind handler returns its response.
	closeTimer := time.NewTimer(2 * time.Second)
	defer closeTimer.Stop()
	var closeReason session.CloseReason
	select {
	case closeReason = <-closeEvent:
	case <-ctx.Done():
		err := fmt.Errorf("uid %q previous session %d close callback: %w", uid, previousID, ctx.Err())
		return nil, apierrors.InternalServer("PIT-447", err.Error(), "").WithCause(err)
	case <-closeTimer.C:
		err := fmt.Errorf("uid %q previous session %d close callback timed out", uid, previousID)
		return nil, apierrors.InternalServer("PIT-447", err.Error(), "").WithCause(err)
	}
	if closeReason != session.CloseReasonNormal && closeReason != session.CloseReasonKickRebind {
		err := fmt.Errorf("uid %q previous session %d close reason = %v, want normal or rebind", uid, previousID, closeReason)
		return nil, apierrors.InternalServer("PIT-447", err.Error(), "").WithCause(err)
	}
	return []byte(fmt.Sprintf("%d|%d|%d", previousID, s.ID(), closeReason)), nil
}

// TestAssertSessionOwnership verifies that the caller is the pool owner for uid.
func (t *TestSvc) TestAssertSessionOwnership(ctx context.Context, byteUID []byte) ([]byte, error) {
	s := t.app.GetSessionFromCtx(ctx)
	if s == nil {
		return nil, apierrors.NotFound("PIT-404", constants.ErrSessionNotFound.Error(), "").
			WithCause(constants.ErrSessionNotFound)
	}
	uid := string(byteUID)
	owner := t.sessionPool.GetSessionByUID(uid)
	if s.UID() != uid || owner == nil || owner.ID() != s.ID() {
		err := fmt.Errorf("session %d does not own uid %q", s.ID(), uid)
		return nil, apierrors.InternalServer("PIT-445", err.Error(), "").WithCause(err)
	}
	return []byte(fmt.Sprintf("%s|%d", uid, s.ID())), nil
}

// TestSessionStats returns authoritative pool counts for stress barriers.
func (t *TestSvc) TestSessionStats(ctx context.Context) ([]byte, error) {
	return []byte(fmt.Sprintf("%d|%d", t.sessionPool.GetSessionCount(), t.sessionPool.GetUserCount())), nil
}

// TestSendGroupMsg handler for e2e tests
func (t *TestSvc) TestSendGroupMsg(ctx context.Context, msg []byte) {
	t.app.GroupBroadcast(ctx, "connector", "g1", "route.test", msg)
}

// TestSendGroupMsgPtr handler for e2e tests
func (t *TestSvc) TestSendGroupMsgPtr(ctx context.Context, msg *test.TestRequest) {
	t.app.GroupBroadcast(ctx, "connector", "g1", "route.testptr", msg)
}

// TestSendToUsers handler for e2e tests
func (t *TestSvc) TestSendToUsers(ctx context.Context, msg *TestSendToUsers) {
	t.app.SendPushToUsers("route.sendtousers", []byte(msg.Msg), msg.UIDs, "connector")
}

// TestSendRPC tests sending a RPC
func (t *TestSvc) TestSendRPC(ctx context.Context, msg *TestRPCRequest) (*protos.TestResponse, error) {
	rep := &protos.TestResponse{}
	err := t.app.RPC(ctx, msg.Route, rep, &protos.TestRequest{Msg: msg.Data}, "")
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// TestSendRPCNoArgs tests sending a RPC
func (t *TestSvc) TestSendRPCNoArgs(ctx context.Context, msg *TestRPCRequest) (*protos.TestResponse, error) {
	rep := &protos.TestResponse{}
	err := t.app.RPC(ctx, msg.Route, rep, nil, "")
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// var app pitaya.Pitaya

func main() {
	port := flag.Int("port", 32222, "the port to listen")
	svType := flag.String("type", "connector", "the server type")
	isFrontend := flag.Bool("frontend", true, "if server is frontend")
	serializer := flag.String("serializer", "json", "json or protobuf")
	sdPrefix := flag.String("sdprefix", "pitaya/", "prefix to discover other servers")
	debug := flag.Bool("debug", false, "turn on debug logging")
	grpc := flag.Bool("grpc", false, "turn on grpc")
	grpcPort := flag.Int("grpcport", 3434, "the grpc server port")
	natsurl := flag.String("natsurl", "nats://localhost:4222", "the nats server addr")
	etcdurl := flag.String("etcdurl", "localhost:2379", "the etcd server addr")
	redisurl := flag.String("redisurl", "localhost:6379", "the redis server addr")
	readyFile := flag.String("readyfile", "", "write the bound frontend address after startup")

	flag.Parse()

	etcdEndpoints := strings.Split(*etcdurl, ",")
	redisAddrs := strings.Split(*redisurl, ",")
	cfg := viper.New()
	cfg.Set("pitaya.cluster.sd.etcd.prefix", *sdPrefix)
	cfg.Set("pitaya.cluster.rpc.server.grpc.port", *grpcPort)
	cfg.Set("pitaya.cluster.rpc.server.nats.connect", *natsurl)
	cfg.Set("pitaya.cluster.rpc.client.nats.connect", *natsurl)
	cfg.Set("pitaya.cluster.sd.etcd.endpoints", etcdEndpoints)
	cfg.Set("pitaya.groups.etcd.endpoints", etcdEndpoints)
	cfg.Set("pitaya.modules.bindingstorage.etcd.endpoints", etcdEndpoints)
	cfg.Set("pitaya.storage.redis.addrs", redisAddrs)
	cfg.Set("pitaya.worker.redis.url", redisAddrs)
	logLevel := "INFO"
	if *debug {
		logLevel = "DEBUG"
	}
	cfg.Set("pitaya.log.development", *debug)
	cfg.Set("pitaya.log.level", logLevel)

	app, bs, sessionPool, tcpAcceptor := createApp(*serializer, *port, *grpc, *isFrontend, *svType, pitaya.Cluster, map[string]string{
		constants.GRPCHostKey: "127.0.0.1",
		constants.GRPCPortKey: fmt.Sprintf("%d", *grpcPort),
	}, cfg)

	if *grpc {
		app.RegisterModule(bs, "bindingsStorage")
	}

	app.Register(
		&TestSvc{
			app:         app,
			sessionPool: sessionPool,
		},
		component.WithName("testsvc"),
		component.WithNameFunc(strings.ToLower),
	)

	app.RegisterRemote(
		&TestRemoteSvc{},
		component.WithName("testremotesvc"),
		component.WithNameFunc(strings.ToLower),
	)
	if *readyFile != "" {
		app.OnStarted(func() {
			address := "ready"
			if tcpAcceptor != nil {
				address = tcpAcceptor.GetAddr()
			}
			if err := writeReadyFile(*readyFile, address); err != nil {
				fmt.Fprintf(os.Stderr, "write readiness file: %v\n", err)
				os.Exit(1)
			}
		})
	}

	app.Start()
}

func createApp(serializer string, port int, grpc bool, isFrontend bool, svType string, serverMode pitaya.ServerMode, metadata map[string]string, cfg ...*viper.Viper) (pitaya.Pitaya, *modules.ETCDBindingStorage, session.SessionPool, *acceptor.TCPAcceptor) {
	conf := config.NewConfig(cfg...)
	builder := pitaya.NewBuilderWithConfigs(isFrontend, svType, serverMode, metadata, conf)

	var tcpAcceptor *acceptor.TCPAcceptor
	if isFrontend {
		tcpAcceptor = acceptor.NewTCPAcceptor(fmt.Sprintf(":%d", port))
		builder.AddAcceptor(tcpAcceptor)
	}

	builder.Groups = groups.NewMemoryGroupService(builder.Config.Groups.Memory)

	if serializer == "json" {
		builder.Serializer = json.NewSerializer()
	} else if serializer == "protobuf" {
		builder.Serializer = protobuf.NewSerializer()
	} else {
		panic("serializer should be either json or protobuf")
	}

	pitayaConfig := config.NewPitayaConfig(conf)
	verifyTestNATSMessagesBuffer(pitayaConfig)

	var bs *modules.ETCDBindingStorage
	if grpc {
		gs, err := cluster.NewGRPCServer(pitayaConfig.Cluster.RPC.Server.Grpc, builder.Server, builder.MetricsReporters)
		if err != nil {
			panic(err)
		}

		bs = modules.NewETCDBindingStorage(builder.Server, builder.SessionPool, pitayaConfig.Modules.BindingStorage.Etcd)

		gc, err := cluster.NewGRPCClient(
			pitayaConfig.Cluster.RPC.Client.Grpc,
			builder.Server,
			builder.MetricsReporters,
			bs,
			cluster.NewInfoRetriever(pitayaConfig.Cluster.Info),
		)
		if err != nil {
			panic(err)
		}
		builder.RPCServer = gs
		builder.RPCClient = gc
	}

	return builder.Build(), bs, builder.SessionPool, tcpAcceptor
}

func verifyTestNATSMessagesBuffer(pitayaConfig *config.PitayaConfig) {
	value := os.Getenv("PITAYA_CLUSTER_RPC_SERVER_NATS_BUFFER_MESSAGES")
	if value == "" {
		return
	}
	expected, err := strconv.Atoi(value)
	if err != nil || expected <= 0 {
		panic(fmt.Sprintf("invalid test NATS messages buffer %q", value))
	}
	actual := pitayaConfig.Cluster.RPC.Server.Nats.Buffer.Messages
	if actual != expected {
		panic(fmt.Sprintf("effective test NATS messages buffer = %d, want %d", actual, expected))
	}
	fmt.Fprintf(os.Stderr, "pitaya test effective NATS messages buffer: %d\n", actual)
}

func writeReadyFile(path, address string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pitaya-ready-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := fmt.Fprintln(tmp, address); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
