package docgenerator

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"strings"

	"github.com/topfreegames/pitaya/v2/constants"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ProtoDescriptors returns the descriptor for a given message name or .proto file
func ProtoDescriptors(protoName string) ([]byte, error) {
	if strings.HasSuffix(protoName, ".proto") {
		desc, err := protoregistry.GlobalFiles.FindFileByPath(protoName)
		if err != nil {
			return nil, err
		}
		descriptor, _ := protodesc.ToFileDescriptorProto(desc).Descriptor()
		if descriptor == nil {
			return nil, constants.ErrProtodescriptor
		}
		return descriptor, nil
	}

	if strings.HasPrefix(protoName, "types.") {
		protoName = strings.Replace(protoName, "types.", "google.protobuf.", 1)
	}
	protoReflectTypePointer, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(protoName))
	if err != nil {
		return nil, err
	}
	protoReflectType := protoReflectTypePointer.Descriptor()
	protoDescriptor, _ := protodesc.ToDescriptorProto(protoReflectType).Descriptor()
	if protoDescriptor == nil {
		return nil, constants.ErrProtodescriptor
	}
	return protoDescriptor, nil
}
