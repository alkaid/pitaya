package networkentity

import (
	"context"
	"net"

	"github.com/topfreegames/pitaya/v2/protos"
)

// NetworkEntity represent low-level network instance
type NetworkEntity interface {
	Push(route string, v interface{}) error
	ResponseMID(ctx context.Context, mid uint, v interface{}, isError ...bool) error
	Close(callback map[string]string, reason ...int) error
	Kick(ctx context.Context) error
	RemoteAddr() net.Addr
	SendRequest(ctx context.Context, serverID, route string, v interface{}) (*protos.Response, error)
}
