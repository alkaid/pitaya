// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.2.0
// - protoc             v3.19.1
// source: pitaya-protos/pitaya.proto

package protos

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// PitayaClient is the client API for Pitaya service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type PitayaClient interface {
	Call(ctx context.Context, in *Request, opts ...grpc.CallOption) (*Response, error)
	PushToUser(ctx context.Context, in *Push, opts ...grpc.CallOption) (*Response, error)
	// Deprecated:上层的bind广播方法已弃用改为Fork实现,所以此处的响应也不再需要了
	SessionBindRemote(ctx context.Context, in *BindMsg, opts ...grpc.CallOption) (*Response, error)
	KickUser(ctx context.Context, in *KickMsg, opts ...grpc.CallOption) (*KickAnswer, error)
}

type pitayaClient struct {
	cc grpc.ClientConnInterface
}

func NewPitayaClient(cc grpc.ClientConnInterface) PitayaClient {
	return &pitayaClient{cc}
}

func (c *pitayaClient) Call(ctx context.Context, in *Request, opts ...grpc.CallOption) (*Response, error) {
	out := new(Response)
	err := c.cc.Invoke(ctx, "/protos.Pitaya/Call", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pitayaClient) PushToUser(ctx context.Context, in *Push, opts ...grpc.CallOption) (*Response, error) {
	out := new(Response)
	err := c.cc.Invoke(ctx, "/protos.Pitaya/PushToUser", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pitayaClient) SessionBindRemote(ctx context.Context, in *BindMsg, opts ...grpc.CallOption) (*Response, error) {
	out := new(Response)
	err := c.cc.Invoke(ctx, "/protos.Pitaya/SessionBindRemote", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pitayaClient) KickUser(ctx context.Context, in *KickMsg, opts ...grpc.CallOption) (*KickAnswer, error) {
	out := new(KickAnswer)
	err := c.cc.Invoke(ctx, "/protos.Pitaya/KickUser", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PitayaServer is the server API for Pitaya service.
// All implementations must embed UnimplementedPitayaServer
// for forward compatibility
type PitayaServer interface {
	Call(context.Context, *Request) (*Response, error)
	PushToUser(context.Context, *Push) (*Response, error)
	// Deprecated:上层的bind广播方法已弃用改为Fork实现,所以此处的响应也不再需要了
	SessionBindRemote(context.Context, *BindMsg) (*Response, error)
	KickUser(context.Context, *KickMsg) (*KickAnswer, error)
	mustEmbedUnimplementedPitayaServer()
}

// UnimplementedPitayaServer must be embedded to have forward compatible implementations.
type UnimplementedPitayaServer struct {
}

func (UnimplementedPitayaServer) Call(context.Context, *Request) (*Response, error) {
	return nil, status.Errorf(codes.Unimplemented, "method Call not implemented")
}
func (UnimplementedPitayaServer) PushToUser(context.Context, *Push) (*Response, error) {
	return nil, status.Errorf(codes.Unimplemented, "method PushToUser not implemented")
}
func (UnimplementedPitayaServer) SessionBindRemote(context.Context, *BindMsg) (*Response, error) {
	return nil, status.Errorf(codes.Unimplemented, "method SessionBindRemote not implemented")
}
func (UnimplementedPitayaServer) KickUser(context.Context, *KickMsg) (*KickAnswer, error) {
	return nil, status.Errorf(codes.Unimplemented, "method KickUser not implemented")
}
func (UnimplementedPitayaServer) mustEmbedUnimplementedPitayaServer() {}

// UnsafePitayaServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to PitayaServer will
// result in compilation errors.
type UnsafePitayaServer interface {
	mustEmbedUnimplementedPitayaServer()
}

func RegisterPitayaServer(s grpc.ServiceRegistrar, srv PitayaServer) {
	s.RegisterService(&Pitaya_ServiceDesc, srv)
}

func _Pitaya_Call_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(Request)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PitayaServer).Call(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/protos.Pitaya/Call",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(PitayaServer).Call(ctx, req.(*Request))
	}
	return interceptor(ctx, in, info, handler)
}

func _Pitaya_PushToUser_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(Push)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PitayaServer).PushToUser(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/protos.Pitaya/PushToUser",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(PitayaServer).PushToUser(ctx, req.(*Push))
	}
	return interceptor(ctx, in, info, handler)
}

func _Pitaya_SessionBindRemote_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(BindMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PitayaServer).SessionBindRemote(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/protos.Pitaya/SessionBindRemote",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(PitayaServer).SessionBindRemote(ctx, req.(*BindMsg))
	}
	return interceptor(ctx, in, info, handler)
}

func _Pitaya_KickUser_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(KickMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PitayaServer).KickUser(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/protos.Pitaya/KickUser",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(PitayaServer).KickUser(ctx, req.(*KickMsg))
	}
	return interceptor(ctx, in, info, handler)
}

// Pitaya_ServiceDesc is the grpc.ServiceDesc for Pitaya service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var Pitaya_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "protos.Pitaya",
	HandlerType: (*PitayaServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Call",
			Handler:    _Pitaya_Call_Handler,
		},
		{
			MethodName: "PushToUser",
			Handler:    _Pitaya_PushToUser_Handler,
		},
		{
			MethodName: "SessionBindRemote",
			Handler:    _Pitaya_SessionBindRemote_Handler,
		},
		{
			MethodName: "KickUser",
			Handler:    _Pitaya_KickUser_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "pitaya-protos/pitaya.proto",
}
