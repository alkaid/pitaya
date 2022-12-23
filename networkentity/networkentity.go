package networkentity

import (
	"context"
	"net"
	"net/netip"

	"github.com/topfreegames/pitaya/v2/protos"
)

// NetworkEntity represent low-level network instance
type NetworkEntity interface {
	Push(route string, v interface{}) error
	ResponseMID(ctx context.Context, mid uint, v interface{}, isError ...bool) error
	Close(callback map[string]string, reason ...int) error
	Kick(ctx context.Context, reason ...int) error
	RemoteAddr() net.Addr
	RemoteIP() netip.Addr
	SendRequest(ctx context.Context, serverID, route string, v interface{}) (*protos.Response, error)
}
