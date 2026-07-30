package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/cluster"
	clustermocks "github.com/alkaid/pitaya/v2/cluster/mocks"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/session"
	"go.uber.org/mock/gomock"
)

type benchmarkFanoutRPCClient struct {
	responses map[string]*protos.Response
}

func (*benchmarkFanoutRPCClient) Send(string, []byte) error { return nil }

func (*benchmarkFanoutRPCClient) SendPush(string, *cluster.Server, *protos.Push) error { return nil }

func (*benchmarkFanoutRPCClient) SendKick(string, string, *protos.KickMsg) error { return nil }

func (*benchmarkFanoutRPCClient) BroadcastSessionBind(string) error { return nil }

func (c *benchmarkFanoutRPCClient) Call(
	_ context.Context,
	_ protos.RPCType,
	_ *route.Route,
	_ session.Session,
	_ *message.Message,
	server *cluster.Server,
) (*protos.Response, error) {
	return c.responses[server.ID], nil
}

func (*benchmarkFanoutRPCClient) Fork(context.Context, *route.Route, session.Session, *message.Message) error {
	return nil
}

func (*benchmarkFanoutRPCClient) Publish(
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

func (*benchmarkFanoutRPCClient) Init() error     { return nil }
func (*benchmarkFanoutRPCClient) AfterInit()      {}
func (*benchmarkFanoutRPCClient) BeforeShutdown() {}
func (*benchmarkFanoutRPCClient) Shutdown() error { return nil }

func BenchmarkDoForkRequestFanout(b *testing.B) {
	for _, targetCount := range []int{32, 128, 1024} {
		b.Run(fmt.Sprintf("targets_%d", targetCount), func(b *testing.B) {
			ctrl := gomock.NewController(b)
			sd := clustermocks.NewMockServiceDiscovery(ctrl)
			self := &cluster.Server{ID: "self", Type: "room"}
			servers := map[string]*cluster.Server{"self": self}
			responses := make(map[string]*protos.Response, targetCount)
			for i := 0; i < targetCount; i++ {
				id := fmt.Sprintf("server-%04d", i)
				servers[id] = &cluster.Server{ID: id, Type: "room"}
				responses[id] = &protos.Response{Data: []byte(id)}
			}
			sd.EXPECT().GetServersByType("room").Return(servers, nil).AnyTimes()
			rpcClient := &benchmarkFanoutRPCClient{responses: responses}
			svc := NewRemoteService(
				rpcClient, nil, sd, nil, nil, nil, nil,
				self, nil, nil, nil, nil,
			)
			targetRoute := route.NewRoute("room", "table", "sync")

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				fanoutResponses, err := svc.DoForkRequest(context.Background(), targetRoute, nil, nil)
				if err != nil {
					b.Fatal(err)
				}
				if len(fanoutResponses) != targetCount {
					b.Fatalf("got %d responses, want %d", len(fanoutResponses), targetCount)
				}
			}
			b.ReportMetric(float64(targetCount), "targets/op")
		})
	}
}

func BenchmarkDoPublishRequestFanoutSkewedTypes(b *testing.B) {
	const (
		denseTargetCount   = 1024
		singletonTypeCount = 1024
	)

	ctrl := gomock.NewController(b)
	sd := clustermocks.NewMockServiceDiscovery(ctrl)
	self := &cluster.Server{ID: "self", Type: "gateway"}
	serverTypes := make(map[string]*cluster.Server, singletonTypeCount+1)
	serversByType := make(map[string]map[string]*cluster.Server, singletonTypeCount+1)
	responses := make(map[string]*protos.Response, denseTargetCount+singletonTypeCount)

	denseRepresentative := &cluster.Server{
		ID:        "dense-representative",
		Type:      "dense",
		Subscribe: make(map[string]*cluster.SubscribeItem),
	}
	denseRepresentative.AddSubscribe("events", "refresh", true)
	serverTypes[denseRepresentative.Type] = denseRepresentative
	denseServers := make(map[string]*cluster.Server, denseTargetCount)
	for i := 0; i < denseTargetCount; i++ {
		id := fmt.Sprintf("dense-%04d", i)
		denseServers[id] = &cluster.Server{ID: id, Type: denseRepresentative.Type}
		responses[id] = &protos.Response{Data: []byte(id)}
	}
	serversByType[denseRepresentative.Type] = denseServers

	for i := 0; i < singletonTypeCount; i++ {
		serverType := fmt.Sprintf("singleton-%04d", i)
		id := serverType + "-server"
		representative := &cluster.Server{
			ID:        id,
			Type:      serverType,
			Subscribe: make(map[string]*cluster.SubscribeItem),
		}
		representative.AddSubscribe("events", "refresh", true)
		serverTypes[serverType] = representative
		serversByType[serverType] = map[string]*cluster.Server{
			id: {ID: id, Type: serverType},
		}
		responses[id] = &protos.Response{Data: []byte(id)}
	}

	sd.EXPECT().GetServerTypes().Return(serverTypes).AnyTimes()
	sd.EXPECT().GetServersByType(gomock.Any()).DoAndReturn(
		func(serverType string) (map[string]*cluster.Server, error) {
			return serversByType[serverType], nil
		},
	).AnyTimes()
	rpcClient := &benchmarkFanoutRPCClient{responses: responses}
	svc := NewRemoteService(
		rpcClient, nil, sd, nil, nil, nil, nil,
		self, nil, nil, nil, nil,
	)
	targetRoute := route.NewRoute("", "events", "refresh")
	targetCount := denseTargetCount + singletonTypeCount

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fanoutResponses, err := svc.DoPublishRequest(context.Background(), targetRoute, nil, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(fanoutResponses) != targetCount {
			b.Fatalf("got %d responses, want %d", len(fanoutResponses), targetCount)
		}
	}
	b.ReportMetric(float64(targetCount), "targets/op")
}
