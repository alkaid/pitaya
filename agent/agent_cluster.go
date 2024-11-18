package agent

import (
	"context"
	"net"
	"net/netip"

	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/util"

	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/serialize"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/session"
	"go.uber.org/zap"
)

// Cluster 云端session(redis)的low networkentity.NetworkEntity
//
//	@implement networkentity.NetworkEntity
type Cluster struct {
	Session          session.Session          // session
	rpcClient        cluster.RPCClient        // rpc client
	serializer       serialize.Serializer     // message serializer
	serviceDiscovery cluster.ServiceDiscovery // service discovery
	uid              string
}

// NewCluster create new Cluster instance
// 数据不存在时返回 [constants.ErrSessionNotFound]
func NewCluster(
	uid string,
	sessionPool session.SessionPool,
	rpcClient cluster.RPCClient,
	serializer serialize.Serializer,
	serviceDiscovery cluster.ServiceDiscovery,
) (*Cluster, error) {
	a := &Cluster{
		serializer:       serializer,
		rpcClient:        rpcClient,
		serviceDiscovery: serviceDiscovery,
	}
	s, _ := sessionPool.NewSession(a, false, uid)
	// cluster类型session的数据来源为云端缓存
	err := s.ObtainFromCluster()
	if err != nil {
		return nil, err
	}
	a.Session = s
	return a, nil
}

func (a *Cluster) NetworkEntityName() string {
	return "Cluster"
}

// Kick kicks the user
func (a *Cluster) Kick(ctx context.Context, reason ...session.CloseReason) error {
	logger.Zap.Error("not support in cluster session! use app.SendKickToUsers instead", zap.Error(constants.ErrNotImplemented))
	return nil
}

// Push pushes the message to the user
func (a *Cluster) Push(route string, v interface{}) error {
	logger.Zap.Error("not support in cluster session! use app.SendPushToUsers instead", zap.Error(constants.ErrNotImplemented))
	return nil
}

// ResponseMID reponds the message with mid to the user
func (a *Cluster) ResponseMID(ctx context.Context, mid uint, v interface{}, isError ...bool) error {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

// Close closes the remote
func (a *Cluster) Close(callback map[string]string, reason ...session.CloseReason) error {
	// logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	logger.Zap.Debug("do nothing in cluster session")
	return nil
}

// RemoteAddr returns the remote address of the user
func (a *Cluster) RemoteAddr() net.Addr {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

func (a *Cluster) RemoteIP() netip.Addr {
	return netip.Addr{}
}

// SendRequest sends a request to a server
func (a *Cluster) SendRequest(ctx context.Context, serverID, reqRoute string, v interface{}) (*protos.Response, error) {
	r, err := route.Decode(reqRoute)
	if err != nil {
		return nil, err
	}
	payload, err := util.SerializeOrRaw(a.serializer, v)
	if err != nil {
		return nil, err
	}
	msg := &message.Message{
		Route: reqRoute,
		Data:  payload,
	}
	server, err := a.serviceDiscovery.GetServer(serverID)
	if err != nil {
		return nil, err
	}
	return a.rpcClient.Call(ctx, protos.RPCType_User, r, nil, msg, server)
}
