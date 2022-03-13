// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.27.1
// 	protoc        v3.19.1
// source: pitaya-protos/bind.proto

package protos

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type BindMsg struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Uid      string            `protobuf:"bytes,1,opt,name=uid,proto3" json:"uid,omitempty"`
	Fid      string            `protobuf:"bytes,2,opt,name=fid,proto3" json:"fid,omitempty"`
	Sid      int64             `protobuf:"varint,3,opt,name=sid,proto3" json:"sid,omitempty"` // frontend session id
	Session  *Session          `protobuf:"bytes,4,opt,name=session,proto3" json:"session,omitempty"`
	Metadata map[string]string `protobuf:"bytes,5,rep,name=metadata,proto3" json:"metadata,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"` // 用户调用Bind()时的透传数据
}

func (x *BindMsg) Reset() {
	*x = BindMsg{}
	if protoimpl.UnsafeEnabled {
		mi := &file_pitaya_protos_bind_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *BindMsg) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*BindMsg) ProtoMessage() {}

func (x *BindMsg) ProtoReflect() protoreflect.Message {
	mi := &file_pitaya_protos_bind_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use BindMsg.ProtoReflect.Descriptor instead.
func (*BindMsg) Descriptor() ([]byte, []int) {
	return file_pitaya_protos_bind_proto_rawDescGZIP(), []int{0}
}

func (x *BindMsg) GetUid() string {
	if x != nil {
		return x.Uid
	}
	return ""
}

func (x *BindMsg) GetFid() string {
	if x != nil {
		return x.Fid
	}
	return ""
}

func (x *BindMsg) GetSid() int64 {
	if x != nil {
		return x.Sid
	}
	return 0
}

func (x *BindMsg) GetSession() *Session {
	if x != nil {
		return x.Session
	}
	return nil
}

func (x *BindMsg) GetMetadata() map[string]string {
	if x != nil {
		return x.Metadata
	}
	return nil
}

type BindBackendMsg struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Uid      string            `protobuf:"bytes,1,opt,name=uid,proto3" json:"uid,omitempty"`
	Btype    string            `protobuf:"bytes,2,opt,name=btype,proto3" json:"btype,omitempty"`                                                                                               // backend server type
	Bid      string            `protobuf:"bytes,3,opt,name=bid,proto3" json:"bid,omitempty"`                                                                                                   // backend server id
	Metadata map[string]string `protobuf:"bytes,4,rep,name=metadata,proto3" json:"metadata,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"` // 用户调用BindBackend()时的透传数据
}

func (x *BindBackendMsg) Reset() {
	*x = BindBackendMsg{}
	if protoimpl.UnsafeEnabled {
		mi := &file_pitaya_protos_bind_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *BindBackendMsg) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*BindBackendMsg) ProtoMessage() {}

func (x *BindBackendMsg) ProtoReflect() protoreflect.Message {
	mi := &file_pitaya_protos_bind_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use BindBackendMsg.ProtoReflect.Descriptor instead.
func (*BindBackendMsg) Descriptor() ([]byte, []int) {
	return file_pitaya_protos_bind_proto_rawDescGZIP(), []int{1}
}

func (x *BindBackendMsg) GetUid() string {
	if x != nil {
		return x.Uid
	}
	return ""
}

func (x *BindBackendMsg) GetBtype() string {
	if x != nil {
		return x.Btype
	}
	return ""
}

func (x *BindBackendMsg) GetBid() string {
	if x != nil {
		return x.Bid
	}
	return ""
}

func (x *BindBackendMsg) GetMetadata() map[string]string {
	if x != nil {
		return x.Metadata
	}
	return nil
}

var File_pitaya_protos_bind_proto protoreflect.FileDescriptor

var file_pitaya_protos_bind_proto_rawDesc = []byte{
	0x0a, 0x18, 0x70, 0x69, 0x74, 0x61, 0x79, 0x61, 0x2d, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x73, 0x2f,
	0x62, 0x69, 0x6e, 0x64, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x06, 0x70, 0x72, 0x6f, 0x74,
	0x6f, 0x73, 0x1a, 0x1b, 0x70, 0x69, 0x74, 0x61, 0x79, 0x61, 0x2d, 0x70, 0x72, 0x6f, 0x74, 0x6f,
	0x73, 0x2f, 0x73, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22,
	0xe2, 0x01, 0x0a, 0x07, 0x42, 0x69, 0x6e, 0x64, 0x4d, 0x73, 0x67, 0x12, 0x10, 0x0a, 0x03, 0x75,
	0x69, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x75, 0x69, 0x64, 0x12, 0x10, 0x0a,
	0x03, 0x66, 0x69, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x66, 0x69, 0x64, 0x12,
	0x10, 0x0a, 0x03, 0x73, 0x69, 0x64, 0x18, 0x03, 0x20, 0x01, 0x28, 0x03, 0x52, 0x03, 0x73, 0x69,
	0x64, 0x12, 0x29, 0x0a, 0x07, 0x73, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x18, 0x04, 0x20, 0x01,
	0x28, 0x0b, 0x32, 0x0f, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x73, 0x2e, 0x53, 0x65, 0x73, 0x73,
	0x69, 0x6f, 0x6e, 0x52, 0x07, 0x73, 0x65, 0x73, 0x73, 0x69, 0x6f, 0x6e, 0x12, 0x39, 0x0a, 0x08,
	0x6d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x18, 0x05, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x1d,
	0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x73, 0x2e, 0x42, 0x69, 0x6e, 0x64, 0x4d, 0x73, 0x67, 0x2e,
	0x4d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x45, 0x6e, 0x74, 0x72, 0x79, 0x52, 0x08, 0x6d,
	0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x1a, 0x3b, 0x0a, 0x0d, 0x4d, 0x65, 0x74, 0x61, 0x64,
	0x61, 0x74, 0x61, 0x45, 0x6e, 0x74, 0x72, 0x79, 0x12, 0x10, 0x0a, 0x03, 0x6b, 0x65, 0x79, 0x18,
	0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x6b, 0x65, 0x79, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61,
	0x6c, 0x75, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65,
	0x3a, 0x02, 0x38, 0x01, 0x22, 0xc9, 0x01, 0x0a, 0x0e, 0x42, 0x69, 0x6e, 0x64, 0x42, 0x61, 0x63,
	0x6b, 0x65, 0x6e, 0x64, 0x4d, 0x73, 0x67, 0x12, 0x10, 0x0a, 0x03, 0x75, 0x69, 0x64, 0x18, 0x01,
	0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x75, 0x69, 0x64, 0x12, 0x14, 0x0a, 0x05, 0x62, 0x74, 0x79,
	0x70, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x05, 0x62, 0x74, 0x79, 0x70, 0x65, 0x12,
	0x10, 0x0a, 0x03, 0x62, 0x69, 0x64, 0x18, 0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x62, 0x69,
	0x64, 0x12, 0x40, 0x0a, 0x08, 0x6d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x18, 0x04, 0x20,
	0x03, 0x28, 0x0b, 0x32, 0x24, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x73, 0x2e, 0x42, 0x69, 0x6e,
	0x64, 0x42, 0x61, 0x63, 0x6b, 0x65, 0x6e, 0x64, 0x4d, 0x73, 0x67, 0x2e, 0x4d, 0x65, 0x74, 0x61,
	0x64, 0x61, 0x74, 0x61, 0x45, 0x6e, 0x74, 0x72, 0x79, 0x52, 0x08, 0x6d, 0x65, 0x74, 0x61, 0x64,
	0x61, 0x74, 0x61, 0x1a, 0x3b, 0x0a, 0x0d, 0x4d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x45,
	0x6e, 0x74, 0x72, 0x79, 0x12, 0x10, 0x0a, 0x03, 0x6b, 0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x03, 0x6b, 0x65, 0x79, 0x12, 0x14, 0x0a, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x18,
	0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x05, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x3a, 0x02, 0x38, 0x01,
	0x42, 0x1b, 0x5a, 0x08, 0x2e, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x73, 0xaa, 0x02, 0x0e, 0x4e,
	0x50, 0x69, 0x74, 0x61, 0x79, 0x61, 0x2e, 0x50, 0x72, 0x6f, 0x74, 0x6f, 0x73, 0x62, 0x06, 0x70,
	0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_pitaya_protos_bind_proto_rawDescOnce sync.Once
	file_pitaya_protos_bind_proto_rawDescData = file_pitaya_protos_bind_proto_rawDesc
)

func file_pitaya_protos_bind_proto_rawDescGZIP() []byte {
	file_pitaya_protos_bind_proto_rawDescOnce.Do(func() {
		file_pitaya_protos_bind_proto_rawDescData = protoimpl.X.CompressGZIP(file_pitaya_protos_bind_proto_rawDescData)
	})
	return file_pitaya_protos_bind_proto_rawDescData
}

var file_pitaya_protos_bind_proto_msgTypes = make([]protoimpl.MessageInfo, 4)
var file_pitaya_protos_bind_proto_goTypes = []interface{}{
	(*BindMsg)(nil),        // 0: protos.BindMsg
	(*BindBackendMsg)(nil), // 1: protos.BindBackendMsg
	nil,                    // 2: protos.BindMsg.MetadataEntry
	nil,                    // 3: protos.BindBackendMsg.MetadataEntry
	(*Session)(nil),        // 4: protos.Session
}
var file_pitaya_protos_bind_proto_depIdxs = []int32{
	4, // 0: protos.BindMsg.session:type_name -> protos.Session
	2, // 1: protos.BindMsg.metadata:type_name -> protos.BindMsg.MetadataEntry
	3, // 2: protos.BindBackendMsg.metadata:type_name -> protos.BindBackendMsg.MetadataEntry
	3, // [3:3] is the sub-list for method output_type
	3, // [3:3] is the sub-list for method input_type
	3, // [3:3] is the sub-list for extension type_name
	3, // [3:3] is the sub-list for extension extendee
	0, // [0:3] is the sub-list for field type_name
}

func init() { file_pitaya_protos_bind_proto_init() }
func file_pitaya_protos_bind_proto_init() {
	if File_pitaya_protos_bind_proto != nil {
		return
	}
	file_pitaya_protos_session_proto_init()
	if !protoimpl.UnsafeEnabled {
		file_pitaya_protos_bind_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*BindMsg); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_pitaya_protos_bind_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*BindBackendMsg); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_pitaya_protos_bind_proto_rawDesc,
			NumEnums:      0,
			NumMessages:   4,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_pitaya_protos_bind_proto_goTypes,
		DependencyIndexes: file_pitaya_protos_bind_proto_depIdxs,
		MessageInfos:      file_pitaya_protos_bind_proto_msgTypes,
	}.Build()
	File_pitaya_protos_bind_proto = out.File
	file_pitaya_protos_bind_proto_rawDesc = nil
	file_pitaya_protos_bind_proto_goTypes = nil
	file_pitaya_protos_bind_proto_depIdxs = nil
}
