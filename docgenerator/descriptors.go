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

// ProtoDescriptors 根据 protoName 获取对应 protobuf 文件及其引用文件的 descriptor
//  @param protoName
//  @param protoDescs key为proto文件注册路径
//  @return error
func ProtoDescriptors(protoName string, protoDescs map[string]*descriptorpb.FileDescriptorProto) error {
	if strings.HasSuffix(protoName, ".proto") {
		return constants.ErrProtodescriptor
	}
	if protoDescs == nil {
		return constants.ErrProtodescriptorMapEmpty
	}
	protoReflectTypePointer, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(protoName))
	if err != nil {
		return err
	}
	fileDesc := protoReflectTypePointer.Descriptor().ParentFile()
	if fileDesc == nil {
		return constants.ErrProtodescriptor
	}
	recusionImports(fileDesc, protoDescs)
	return nil
}

// recusionImports 递归自己及引用,若没有获取过这设置到 protoDescs map 里
//  @param fileDesc
//  @param protoDescs
func recusionImports(fileDesc protoreflect.FileDescriptor, protoDescs map[string]*descriptorpb.FileDescriptorProto) {
	// 已经获取过 不再递归
	if _, ok := protoDescs[fileDesc.Path()]; ok {
		return
	}
	protoDescriptor := protodesc.ToFileDescriptorProto(fileDesc)
	// 设值给集合
	protoDescs[fileDesc.Path()] = protoDescriptor
	// 便利递归其引用
	for i := 0; i < fileDesc.Imports().Len(); i++ {
		importFileDesc := fileDesc.Imports().Get(i).FileDescriptor
		if importFileDesc == nil {
			logger.Zap.Warn("importFileDesc is nil", zap.String("parent", fileDesc.Path()), zap.Int("import", i))
			continue
		}
		recusionImports(importFileDesc, protoDescs)
	}
}

// ProtoFileDescriptors 根据文件名获取对应 protobuf 文件及其引用文件的 descriptor
//  @param protoName
//  @param protoDescs key为proto文件注册路径
//  @return error
func ProtoFileDescriptors(protoName string, protoDescs map[string]*descriptorpb.FileDescriptorProto) error {
	if !strings.HasSuffix(protoName, ".proto") {
		return constants.ErrProtodescriptor
	}
	if protoDescs == nil {
		return constants.ErrProtodescriptorMapEmpty
	}
	fileDesc, err := protoregistry.GlobalFiles.FindFileByPath(protoName)
	if err != nil {
		return err
	}
	recusionImports(fileDesc, protoDescs)
	return nil
}
