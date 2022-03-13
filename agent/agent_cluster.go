package agent

import (
	"context"
	"net"

	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/session"
	"go.uber.org/zap"
)

// Cluster 云端session(redis)的low networkentity.NetworkEntity
//  @implement networkentity.NetworkEntity
type Cluster struct {
	Session session.Session // session
}

// NewCluster create new Cluster instance
func NewCluster(
	uid string,
	sessionPool session.SessionPool,
) (*Cluster, error) {
	a := &Cluster{}
	s, err := sessionPool.ImperfectSessionFromCluster(uid)
	if err != nil {
		return nil, err
	}
	a.Session = s
	return a, nil
}

// Kick kicks the user
func (c *Cluster) Kick(ctx context.Context) error {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

// Push pushes the message to the user
func (c *Cluster) Push(route string, v interface{}) error {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

// ResponseMID reponds the message with mid to the user
func (c *Cluster) ResponseMID(ctx context.Context, mid uint, v interface{}, isError ...bool) error {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

// Close closes the remote
func (c *Cluster) Close(callback map[string]string, reason ...session.CloseReason) error {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

// RemoteAddr returns the remote address of the user
func (c *Cluster) RemoteAddr() net.Addr {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil
}

// SendRequest sends a request to a server
func (c *Cluster) SendRequest(ctx context.Context, serverID, reqRoute string, v interface{}) (*protos.Response, error) {
	logger.Zap.Error("not support in cluster session!", zap.Error(constants.ErrNotImplemented))
	return nil, constants.ErrNotImplemented
}
