// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package util

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/serialize"
	jsonserializer "github.com/alkaid/pitaya/v2/serialize/json"
	"github.com/alkaid/pitaya/v2/serialize/mocks"
	protobufserializer "github.com/alkaid/pitaya/v2/serialize/protobuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

var update = flag.Bool("update", false, "update .golden files")

type someStruct struct {
	A int
	B string
}

func (s *someStruct) TestFunc(arg1 int, arg2 string) (*someStruct, error) {
	return &someStruct{
		A: arg1,
		B: arg2,
	}, nil
}

func (s *someStruct) TestFuncErr(arg string) (*someStruct, error) {
	return nil, errors.New(arg)
}

func (s *someStruct) TestFuncThrow() (*someStruct, error) {
	panic("ohnoes")
}

func (s *someStruct) TestFuncNil(arg string) (*someStruct, error) {
	return nil, nil
}

func TestPcall(t *testing.T) {
	t.Parallel()
	s := &someStruct{}
	tables := []struct {
		name       string
		obj        interface{}
		methodName string
		args       []reflect.Value
		out        interface{}
		err        error
	}{
		{"test_pcall_1", s, "TestFunc", []reflect.Value{reflect.ValueOf(s), reflect.ValueOf(10), reflect.ValueOf("bla")}, &someStruct{A: 10, B: "bla"}, nil},
		{"test_pcall_2", s, "TestFunc", []reflect.Value{reflect.ValueOf(s), reflect.ValueOf(20), reflect.ValueOf("ble")}, &someStruct{A: 20, B: "ble"}, nil},
		{"test_pcall_3", s, "TestFunc", []reflect.Value{reflect.ValueOf(s), reflect.ValueOf(11), reflect.ValueOf("blb")}, &someStruct{A: 11, B: "blb"}, nil},
		{"test_pcall_4", s, "TestFuncErr", []reflect.Value{reflect.ValueOf(s), reflect.ValueOf("blberror")}, nil, errors.New("blberror")},
		{"test_pcall_5", s, "TestFuncThrow", []reflect.Value{reflect.ValueOf(s)}, nil, errors.New("ohnoes")},
		{"test_pcall_6", s, "TestFuncNil", []reflect.Value{reflect.ValueOf(s), reflect.ValueOf("kkk")}, nil, constants.ErrReplyShouldBeNotNull},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			m, ok := reflect.TypeOf(table.obj).MethodByName(table.methodName)
			assert.True(t, ok)
			r, err := Pcall(m, table.args)
			if table.methodName == "TestFunc" || table.methodName == "TestFunc2RetNoErr" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, table.err.Error())
			}
			assert.IsType(t, table.out, r)
			assert.Equal(t, table.out, r)
		})
	}
}

func TestSliceContainsString(t *testing.T) {
	t.Parallel()
	tables := []struct {
		slice []string
		str   string
		ret   bool
	}{
		{[]string{"bla", "ble", "bli"}, "bla", true},
		{[]string{"bl", "ble", "bli"}, "bla", false},
		{[]string{"b", "a", "c"}, "c", true},
		{[]string{"c"}, "c", true},
		{[]string{"c"}, "d", false},
		{[]string{}, "d", false},
		{nil, "d", false},
	}

	for _, table := range tables {
		t.Run(fmt.Sprintf("slice:%s str:%s", table.slice, table.str), func(t *testing.T) {
			res := SliceContainsString(table.slice, table.str)
			assert.Equal(t, table.ret, res)
		})
	}
}

func TestSerializeOrRaw(t *testing.T) {
	t.Parallel()
	tables := []struct {
		in  interface{}
		out interface{}
	}{
		{[]byte{1, 2, 3}, []byte{1, 2, 3}},
		{[]byte{3, 2, 3}, []byte{3, 2, 3}},
		{"bla", []byte{1}},
		{"ble", []byte{1}},
	}

	for i, table := range tables {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockSerializer := mocks.NewMockSerializer(ctrl)

			if reflect.TypeOf(table.in) != reflect.TypeOf(([]byte)(nil)) {
				mockSerializer.EXPECT().Marshal(table.in).Return(table.out, nil)
			}
			res, err := SerializeOrRaw(mockSerializer, table.in)
			assert.NoError(t, err)
			assert.Equal(t, table.out, res)
		})
	}
}

func TestFileExists(t *testing.T) {
	t.Parallel()
	ins := []struct {
		name string
		out  bool
	}{
		{"gob_encode_test_1", true},
		{"gob_encode_test_2", true},
		{"gob_encode_test_3", true},
		{"gob_encode_test_4", false},
	}

	for _, in := range ins {
		t.Run(in.name, func(t *testing.T) {
			gp := filepath.Join("fixtures", in.name+".golden")
			out := FileExists(gp)
			assert.Equal(t, in.out, out)
		})
	}

}

func TestGetErrorPayload(t *testing.T) {
	t.Parallel()
	tables := []struct {
		in  error
		out []byte
	}{
		{errors.New("some custom error1"), []byte{0x01}},
		{errors.New("error3"), []byte{0x02}},
		{errors.New("bla"), []byte{0x03}},
		{errors.New(""), []byte{0x04}},
	}
	for i, table := range tables {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockSerializer := mocks.NewMockSerializer(ctrl)
			mockSerializer.EXPECT().Marshal(gomock.Any()).Return(table.out, nil)

			b, err := GetErrorPayload(mockSerializer, table.in)
			assert.NoError(t, err)
			assert.Equal(t, table.out, b)
		})
	}
}

func TestGetErrorFromMalformedPayload(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		serializer serialize.Serializer
		payload    []byte
	}{
		"json": {
			serializer: jsonserializer.NewSerializer(),
			payload:    []byte(`{"code":418,"reason":{},"message":"partial"}`),
		},
		"protobuf": {
			serializer: protobufserializer.NewSerializer(),
			payload:    []byte{0xff},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertUnknownErrorPayload(t, test.serializer, test.payload)
		})
	}
}

func TestGetErrorFromPayloadAcceptsUnknownFields(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		serializer := jsonserializer.NewSerializer()
		payload := []byte(`{"code":418,"reason":"teapot","message":"short","unknown":true}`)
		got := apierrors.FromError(GetErrorFromPayload(serializer, payload))
		assert.Equal(t, int32(418), got.Status.Code)
		assert.Equal(t, "teapot", got.Status.Reason)
		assert.Equal(t, "short", got.Status.Message)
	})

	t.Run("protobuf", func(t *testing.T) {
		serializer := protobufserializer.NewSerializer()
		payload, err := serializer.Marshal(&apierrors.Status{
			Code:    503,
			Reason:  "unavailable",
			Message: "retry later",
		})
		require.NoError(t, err)
		payload = append(payload, 0x98, 0x06, 0x01)

		got := apierrors.FromError(GetErrorFromPayload(serializer, payload))
		assert.Equal(t, int32(503), got.Status.Code)
		assert.Equal(t, "unavailable", got.Status.Reason)
		assert.Equal(t, "retry later", got.Status.Message)
	})
}

func FuzzGetErrorFromJSONPayload(f *testing.F) {
	serializer := jsonserializer.NewSerializer()
	seed := apierrors.InternalServer("seed.reason", "seed message", "").WithMetadata(map[string]string{"key": "value"})
	encoded, err := serializer.Marshal(seed)
	require.NoError(f, err)

	f.Add(encoded)
	f.Add([]byte(`{"code":418,"reason":"teapot","message":"short","unknown":true}`))
	f.Add([]byte(`{"code":418,"reason":{},"message":"partial"}`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		assertStableErrorPayload(t, serializer, payload)
		probe := apierrors.InternalServer("probe", "probe", "")
		if err := serializer.Unmarshal(payload, probe); err != nil {
			assertUnknownErrorPayload(t, serializer, payload)
		}
	})
}

func FuzzGetErrorFromProtobufPayload(f *testing.F) {
	serializer := protobufserializer.NewSerializer()
	seed := &apierrors.Status{
		Code:     503,
		Reason:   "seed.reason",
		Message:  "seed message",
		Metadata: map[string]string{"key": "value"},
	}
	encoded, err := serializer.Marshal(seed)
	require.NoError(f, err)

	f.Add(encoded)
	f.Add(append(append([]byte{}, encoded...), 0x98, 0x06, 0x01))
	f.Add([]byte{0x0a, 0xff})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, payload []byte) {
		assertStableErrorPayload(t, serializer, payload)
		if err := serializer.Unmarshal(payload, &apierrors.Status{}); err != nil {
			assertUnknownErrorPayload(t, serializer, payload)
		}
	})
}

func assertUnknownErrorPayload(t *testing.T, serializer serialize.Serializer, payload []byte) {
	t.Helper()
	got := apierrors.FromError(GetErrorFromPayload(serializer, payload))
	assert.Equal(t, int32(500), got.Status.Code)
	assert.Equal(t, "unknown", got.Status.Reason)
	assert.Equal(t, "unknown", got.Status.Message)
}

func assertStableErrorPayload(t *testing.T, serializer serialize.Serializer, payload []byte) {
	t.Helper()
	if len(payload) > 64<<10 {
		t.Skip()
	}

	first := apierrors.FromError(GetErrorFromPayload(serializer, payload))
	second := apierrors.FromError(GetErrorFromPayload(serializer, payload))
	if !reflect.DeepEqual(&first.Status, &second.Status) {
		t.Fatalf("error payload decode is not deterministic: first=%#v second=%#v", &first.Status, &second.Status)
	}
}

func TestConvertProtoToMessageType(t *testing.T) {
	t.Parallel()
	tables := []struct {
		in  protos.MsgType
		out message.Type
	}{
		{protos.MsgType_MsgRequest, message.Request},
		{protos.MsgType_MsgNotify, message.Notify},
	}

	for i, table := range tables {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			out := ConvertProtoToMessageType(table.in)
			assert.Equal(t, table.out, out)
		})
	}
}
