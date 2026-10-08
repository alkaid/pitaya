package docgenerator

import (
	"testing"

	"github.com/alkaid/pitaya/v2/constants"
	_ "github.com/alkaid/pitaya/v2/protos"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestProtoDescriptors(t *testing.T) {
	t.Parallel()
	tables := []struct {
		name      string
		protoName string
		load      func(string, map[string]*descriptorpb.FileDescriptorProto) error
		wantErr   error
		shouldErr bool
	}{
		{"reject filename as message", "not_exists.proto", ProtoDescriptors, constants.ErrProtodescriptor, true},
		{"success filename", "pitaya-protos/kick.proto", ProtoFileDescriptors, nil, false},
		{"success message", "protos.Push", ProtoDescriptors, nil, false},
		{"fail message", "protos.DoNotExist", ProtoDescriptors, nil, true},
	}
	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			descriptors := make(map[string]*descriptorpb.FileDescriptorProto)
			err := table.load(table.protoName, descriptors)
			if table.shouldErr {
				assert.Error(t, err)
				if table.wantErr != nil {
					assert.ErrorIs(t, err, table.wantErr)
				}
				assert.Empty(t, descriptors)
			} else {
				assert.NoError(t, err)
				assert.NotEmpty(t, descriptors)
			}
		})
	}

	t.Run("nil descriptor map", func(t *testing.T) {
		assert.ErrorIs(t, ProtoDescriptors("protos.Push", nil), constants.ErrProtodescriptorMapEmpty)
	})
}
