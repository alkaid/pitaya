package services

import (
	"context"
	"fmt"
	"time"

	"github.com/topfreegames/pitaya/v2"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/examples/demo/cluster_ak/protos"
	"github.com/topfreegames/pitaya/v2/timer"
)

type (
	// Room represents a component that contains a bundle of room related handler
	// like Join/Message
	Room struct {
		component.Base
		timer *timer.Timer
		app   pitaya.Pitaya
		Stats *Stats
	}

	// Stats exports the room status
	Stats struct {
		outboundBytes int
		inboundBytes  int
	}
)

func (r *Room) BeforeShutdown() {
	// TODO implement me
	panic("implement me")
}

func (r *Room) Shutdown() {
	// TODO implement me
	panic("implement me")
}

// Outbound gets the outbound status
func (Stats *Stats) Outbound(ctx context.Context, in []byte) ([]byte, error) {
	Stats.outboundBytes += len(in)
	return in, nil
}

// Inbound gets the inbound status
func (Stats *Stats) Inbound(ctx context.Context, in []byte) ([]byte, error) {
	Stats.inboundBytes += len(in)
	return in, nil
}

// NewRoom returns a new room
func NewRoom(app pitaya.Pitaya) *Room {
	return &Room{
		app:   app,
		Stats: &Stats{},
	}
}

// Init runs on service initialization
func (r *Room) Init() {
	r.app.GroupCreate(context.Background(), "room")
}

// AfterInit component lifetime callback
func (r *Room) AfterInit() {
	r.timer = pitaya.NewTimer(time.Minute, func() {
		count, err := r.app.GroupCountMembers(context.Background(), "room")
		println("UserCount: Time=>", time.Now().String(), "Count=>", count, "Error=>", err)
		println("OutboundBytes", r.Stats.outboundBytes)
		println("InboundBytes", r.Stats.outboundBytes)
	})
}

func reply(code int32, msg string) *protos.Response {
	return &protos.Response{
		Code: code,
		Msg:  msg,
	}
}

// Entry is the entrypoint
func (r *Room) Entry(ctx context.Context, req *protos.LoginReq) (*protos.LoginResponse, error) {
	// fakeUID := uuid.New().String() // just use s.ID as uid !!!
	fakeUID := req.GetToken()
	// s := r.app.GetSessionFromCtx(ctx)
	// err := s.Bind(ctx, fakeUID) // binding session uid
	// if err != nil {
	//	return nil, pitaya.Error(err, "ENT-000")
	// }
	println("onclient entry %v", fakeUID)
	return &protos.LoginResponse{
		Code: 200,
		Msg:  "ok",
		Uid:  fakeUID,
	}, nil
}

// Join room
func (r *Room) Join(ctx context.Context) (*protos.Response, error) {
	s := r.app.GetSessionFromCtx(ctx)
	members, err := r.app.GroupMembers(ctx, "room")
	if err != nil {
		return nil, err
	}
	s.Push("client.onMembers", &protos.AllMembers{Members: members})
	r.app.GroupBroadcast(ctx, "connector", "room", "client.onNewUser", &protos.NewUser{Content: fmt.Sprintf("New user: %d", s.ID())})
	r.app.GroupAddMember(ctx, "room", s.UID())
	s.OnClose(func() {
		r.app.GroupRemoveMember(ctx, "room", s.UID())
	})

	return &protos.Response{Msg: "success"}, nil
}

// Message sync last message to all members
func (r *Room) Message(ctx context.Context, msg *protos.UserMessage) {
	err := r.app.GroupBroadcast(ctx, "connector", "room", "client.onMessage", msg)
	if err != nil {
		fmt.Println("error broadcasting message", err)
	}
}

// SendRPC sends rpc
func (r *Room) SendRPC(ctx context.Context, msg []byte) (*protos.Response, error) {
	ret := protos.Response{}
	err := r.app.RPC(ctx, "connector.connectorremote.remotefunc", &ret, &protos.RPCMsg{}, "")
	if err != nil {
		return nil, err
	}
	return reply(200, ret.Msg), nil
}
