package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type contextCapturingPitayaServer struct {
	protos.UnimplementedPitayaServer
	contexts chan context.Context
}

func (s *contextCapturingPitayaServer) Call(ctx context.Context, _ *protos.Request) (*protos.Response, error) {
	s.contexts <- ctx
	return &protos.Response{}, nil
}

type panicOncePitayaServer struct {
	protos.UnimplementedPitayaServer
	calls atomic.Int32
}

type panicOnLogPitayaServer struct {
	protos.UnimplementedPitayaServer
	calls atomic.Int32
}

type panicOnLogValue struct{}

func (panicOnLogValue) MarshalLogObject(zapcore.ObjectEncoder) error {
	panic("panic while encoding the recovered value")
}

type panicWriteSyncer struct{}

func (panicWriteSyncer) Write([]byte) (int, error) {
	panic("panic while writing the recovery log")
}

func (panicWriteSyncer) Sync() error { return nil }

type blockingPitayaServer struct {
	protos.UnimplementedPitayaServer
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func newBlockingPitayaServer() *blockingPitayaServer {
	return &blockingPitayaServer{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *blockingPitayaServer) Call(context.Context, *protos.Request) (*protos.Response, error) {
	s.startedOnce.Do(func() { close(s.started) })
	<-s.release
	return &protos.Response{}, nil
}

func (s *blockingPitayaServer) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func (s *panicOncePitayaServer) Call(context.Context, *protos.Request) (*protos.Response, error) {
	if s.calls.Add(1) == 1 {
		panic("secret panic details")
	}
	return &protos.Response{}, nil
}

func (s *panicOnLogPitayaServer) Call(context.Context, *protos.Request) (*protos.Response, error) {
	if s.calls.Add(1) == 1 {
		panic(panicOnLogValue{})
	}
	return &protos.Response{}, nil
}

func TestNewGRPCServer(t *testing.T) {
	t.Parallel()
	sv := getServer()
	gs, err := NewGRPCServer(config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc, sv, []metrics.Reporter{})
	assert.NoError(t, err)
	assert.NotNil(t, gs)
}

func TestNewGRPCServerRejectsInvalidPort(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		t.Run(fmt.Sprintf("port_%d", port), func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
			cfg.Port = port
			server, err := NewGRPCServer(cfg, getServer(), nil)
			require.Error(t, err)
			assert.Nil(t, server)
			assert.Contains(t, err.Error(), "pitaya.cluster.rpc.server.grpc.port")
		})
	}
}

func TestGRPCServerUnsupportedSubscribeAndShutdownBeforeInit(t *testing.T) {
	gs, err := NewGRPCServer(
		config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc,
		getServer(),
		nil,
	)
	require.NoError(t, err)
	assert.ErrorIs(t, gs.Subscribe("events", "workers"), constants.ErrNotImplemented)
	gs.AfterInit()
	gs.BeforeShutdown()
	assert.NoError(t, gs.Shutdown())
}

func TestGRPCServerInit(t *testing.T) {
	t.Parallel()
	c := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	c.Port = helpers.GetFreePort(t)
	sv := getServer()
	gs, err := NewGRPCServer(c, sv, []metrics.Reporter{})
	gs.SetPitayaServer(&protos.UnimplementedPitayaServer{})
	err = gs.Init()
	assert.NoError(t, err)
	assert.NotNil(t, gs)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port))
	assert.NoError(t, err)
	assert.NotNil(t, conn)
	assert.NotNil(t, gs.grpcSv)
	require.NoError(t, conn.Close())

	gs.BeforeShutdown()
	assert.NoError(t, gs.Shutdown(), "Shutdown must be idempotent after the admission barrier")
}

func TestGRPCServerCallDecodesPropagatedRequestContext(t *testing.T) {
	c := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	c.Port = helpers.GetFreePort(t)
	gs, err := NewGRPCServer(c, getServer(), nil)
	require.NoError(t, err)
	capture := &contextCapturingPitayaServer{contexts: make(chan context.Context, 1)}
	gs.SetPitayaServer(capture)
	require.NoError(t, gs.Init())
	t.Cleanup(func() { require.NoError(t, gs.Shutdown()) })

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", c.Port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	propagated := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "game-1",
		constants.OwnerEpochKey, "42",
		constants.IdempotencyKey, "worker-jid-1",
	)
	metadata, err := pcontext.Encode(propagated)
	require.NoError(t, err)
	callCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = protos.NewPitayaClient(conn).Call(callCtx, &protos.Request{Metadata: metadata})
	require.NoError(t, err)

	select {
	case received := <-capture.contexts:
		assert.Equal(t, "game-1", pcontext.GetFromPropagateCtx(received, constants.OwnerServerIDKey))
		assert.Equal(t, "42", pcontext.GetFromPropagateCtx(received, constants.OwnerEpochKey))
		assert.Equal(t, "worker-jid-1", pcontext.GetFromPropagateCtx(received, constants.IdempotencyKey))
	case <-callCtx.Done():
		t.Fatal("timed out waiting for gRPC Call context")
	}
}

func TestGRPCServerRecoversPitayaCallPanic(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	cfg.Port = helpers.GetFreePort(t)
	gs, err := NewGRPCServer(cfg, getServer(), nil)
	require.NoError(t, err)
	gs.SetPitayaServer(&panicOncePitayaServer{})
	require.NoError(t, gs.Init())
	t.Cleanup(func() { require.NoError(t, gs.Shutdown()) })

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := protos.NewPitayaClient(conn)
	callCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	metadata, err := pcontext.Encode(pcontext.AddToPropagateCtx(context.Background(), "test-key", "test-value"))
	require.NoError(t, err)
	request := &protos.Request{Metadata: metadata}

	_, err = client.Call(callCtx, request)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.NotContains(t, err.Error(), "secret panic details")

	_, err = client.Call(callCtx, request)
	require.NoError(t, err, "server must continue serving after a recovered handler panic")
}

func TestGRPCServerRecoveryDoesNotEncodeRecoveredValue(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	cfg.Port = helpers.GetFreePort(t)
	gs, err := NewGRPCServer(cfg, getServer(), nil)
	require.NoError(t, err)
	gs.SetPitayaServer(&panicOnLogPitayaServer{})
	require.NoError(t, gs.Init())
	t.Cleanup(func() { require.NoError(t, gs.Shutdown()) })

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := protos.NewPitayaClient(conn)
	callCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	metadata, err := pcontext.Encode(pcontext.AddToPropagateCtx(context.Background(), "test-key", "test-value"))
	require.NoError(t, err)
	request := &protos.Request{Metadata: metadata}

	_, err = client.Call(callCtx, request)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.NotContains(t, err.Error(), "panic while encoding the recovered value")

	_, err = client.Call(callCtx, request)
	require.NoError(t, err, "server must continue after a recovered value rejects log encoding")
}

func TestGRPCServerRecoverySurvivesPanickingLogger(t *testing.T) {
	originalLogger := logger.Zap
	logger.Zap = zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		panicWriteSyncer{},
		zapcore.ErrorLevel,
	))
	t.Cleanup(func() { logger.Zap = originalLogger })

	gs := &GRPCServer{}
	response, err := gs.requestContextInterceptor(
		context.Background(),
		struct{}{},
		&grpc.UnaryServerInfo{FullMethod: "/test.Pitaya/Call"},
		func(context.Context, any) (any, error) {
			panic("handler panic")
		},
	)

	assert.Nil(t, response)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

func openStalledGRPCStream(t *testing.T, address string) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", address, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	framer := http2.NewFramer(conn, conn)

	_, err = io.WriteString(conn, http2.ClientPreface)
	require.NoError(t, err)
	require.NoError(t, framer.WriteSettings())

	var headerBlock bytes.Buffer
	encoder := hpack.NewEncoder(&headerBlock)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: protos.Pitaya_Call_FullMethodName},
		{Name: ":authority", Value: address},
		{Name: "content-type", Value: "application/grpc"},
		{Name: "te", Value: "trailers"},
	} {
		require.NoError(t, encoder.WriteField(field))
	}
	require.NoError(t, framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: headerBlock.Bytes(),
		EndHeaders:    true,
	}))

	ping := [8]byte{0x70, 0x69, 0x74, 0x61, 0x79, 0x61, 0x2d, 0x32}
	require.NoError(t, framer.WritePing(false, ping))
	for {
		frame, err := framer.ReadFrame()
		require.NoError(t, err)
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				require.NoError(t, framer.WriteSettingsAck())
			}
		case *http2.PingFrame:
			if frame.Flags.Has(http2.FlagPingAck) && frame.Data == ping {
				require.NoError(t, conn.SetDeadline(time.Time{}))
				return conn
			}
		case *http2.RSTStreamFrame:
			if frame.StreamID == 1 {
				conn.Close()
				t.Fatalf("gRPC server rejected stalled stream: %v", frame.ErrCode)
			}
		case *http2.GoAwayFrame:
			conn.Close()
			t.Fatalf("gRPC server rejected HTTP/2 connection: %v", frame.ErrCode)
		}
	}
}

func TestGRPCServerGracefulStopFallsBackToStop(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	cfg.Port = helpers.GetFreePort(t)
	gs, err := NewGRPCServer(cfg, getServer(), nil)
	require.NoError(t, err)
	gs.gracefulStopTimeout = 50 * time.Millisecond
	blockingServer := newBlockingPitayaServer()
	t.Cleanup(blockingServer.Release)
	gs.SetPitayaServer(blockingServer)
	require.NoError(t, gs.Init())
	t.Cleanup(func() { require.NoError(t, gs.Shutdown()) })

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	metadata, err := pcontext.Encode(pcontext.AddToPropagateCtx(context.Background(), "test-key", "test-value"))
	require.NoError(t, err)
	callResult := make(chan error, 1)
	go func() {
		_, callErr := protos.NewPitayaClient(conn).Call(context.Background(), &protos.Request{Metadata: metadata})
		callResult <- callErr
	}()

	select {
	case <-blockingServer.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for admitted gRPC call")
	}
	shutdownDone := make(chan struct{})
	go func() {
		gs.BeforeShutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		blockingServer.Release()
		gs.grpcSv.Stop()
		t.Fatal("gRPC shutdown did not fall back to a forced stop")
	}
	select {
	case callErr := <-callResult:
		require.Error(t, callErr, "forced stop must terminate the active client RPC")
	case <-time.After(time.Second):
		blockingServer.Release()
		t.Fatal("client RPC remained active after forced gRPC stop")
	}
}

func TestGRPCServerShutdownDeadlineCoversPreInterceptorStream(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.RPC.Server.Grpc
	cfg.Port = helpers.GetFreePort(t)
	gs, err := NewGRPCServer(cfg, getServer(), nil)
	require.NoError(t, err)
	gs.gracefulStopTimeout = 50 * time.Millisecond
	server := newBlockingPitayaServer()
	gs.SetPitayaServer(server)
	require.NoError(t, gs.Init())
	t.Cleanup(func() {
		gs.grpcSv.Stop()
		_ = gs.Shutdown()
	})

	conn := openStalledGRPCStream(t, fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	select {
	case <-server.started:
		t.Fatal("stalled HTTP/2 stream unexpectedly reached the Pitaya handler")
	default:
	}

	shutdownDone := make(chan struct{})
	go func() {
		gs.BeforeShutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		_ = conn.Close()
		gs.grpcSv.Stop()
		t.Fatal("gRPC shutdown deadline did not cover GracefulStop")
	}
}
