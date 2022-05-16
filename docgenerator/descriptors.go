package docgenerator

import (
	"strings"

	"github.com/topfreegames/pitaya/v2/logger"
	"go.uber.org/zap"

	"google.golang.org/protobuf/types/descriptorpb"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/topfreegames/pitaya/v2/constants"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ProtoDescriptors returns the descriptor for a given message name
func ProtoDescriptors(protoName string) ([]*descriptorpb.FileDescriptorProto, error) {
	if strings.HasSuffix(protoName, ".proto") {
		return nil, constants.ErrProtodescriptor
	}
	// 不返回protoc include库，默认客户端自己有该库
	if strings.HasPrefix(protoName, "google.protobuf") {
		return nil, nil
	}
	var descs []*descriptorpb.FileDescriptorProto
	protoReflectTypePointer, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(protoName))
	if err != nil {
		return nil, err
	}
	fileDesc := protoReflectTypePointer.Descriptor().ParentFile()
	if fileDesc == nil {
		return nil, constants.ErrProtodescriptor
	}
	protoDescriptor := protodesc.ToFileDescriptorProto(fileDesc)
	descs = append(descs, protoDescriptor)
	for i := 0; i < fileDesc.Imports().Len(); i++ {
		importFileDesc := fileDesc.Imports().Get(i).FileDescriptor
		if importFileDesc == nil {
			logger.Zap.Warn("importFileDesc is nil", zap.String("protoName", protoName), zap.Int("import", i))
			continue
		}
		importDescProto := protodesc.ToFileDescriptorProto(importFileDesc)
		descs = append(descs, importDescProto)
	}
	return descs, nil
}

// ProtoFileDescriptors returns the descriptor for a given .proto file
func ProtoFileDescriptors(protoName string) ([]*descriptorpb.FileDescriptorProto, error) {
	if !strings.HasSuffix(protoName, ".proto") {
		return nil, constants.ErrProtodescriptor
	}
	var descs []*descriptorpb.FileDescriptorProto
	fileDesc, err := protoregistry.GlobalFiles.FindFileByPath(protoName)
	if err != nil {
		return nil, err
	}
	descriptor := protodesc.ToFileDescriptorProto(fileDesc)
	if descriptor == nil {
		return nil, constants.ErrProtodescriptor
	}
	descs = append(descs, descriptor)
	for i := 0; i < fileDesc.Imports().Len(); i++ {
		importFileDesc := fileDesc.Imports().Get(i).FileDescriptor
		if importFileDesc == nil {
			logger.Zap.Warn("importFileDesc is nil", zap.String("protoName", protoName), zap.Int("import", i))
			continue
		}
		importDescProto := protodesc.ToFileDescriptorProto(importFileDesc)
		descs = append(descs, importDescProto)
	}
	return descs, nil
}
