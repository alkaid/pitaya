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

package client

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	connmocks "github.com/alkaid/pitaya/v2/mocks"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func testDescriptorProto() *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:    proto.String("client_fixture.proto"),
		Package: proto.String("fixture"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Input"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("value"),
						JsonName: proto.String("value"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
				},
			},
			{
				Name: proto.String("Output"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("result"),
						JsonName: proto.String("result"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
				},
			},
		},
	}
}

func testDescriptor(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	descriptor, err := protodesc.NewFile(testDescriptorProto(), nil)
	require.NoError(t, err)
	return descriptor
}

func gzipPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, err := w.Write(payload)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return compressed.Bytes()
}

func decodeWireMessage(t *testing.T, frame []byte) *message.Message {
	t.Helper()
	packets, err := codec.NewPomeloPacketDecoder().Decode(frame)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	require.Equal(t, packet.Type(packet.Data), packets[0].Type)
	msg, err := message.Decode(packets[0].Data)
	require.NoError(t, err)
	return msg
}

func TestUnpackDescriptor(t *testing.T) {
	descriptor := testDescriptorProto()
	encoded, err := proto.Marshal(descriptor)
	require.NoError(t, err)

	got, err := unpackDescriptor(gzipPayload(t, encoded))
	require.NoError(t, err)
	assert.True(t, proto.Equal(descriptor, got))

	got, err = unpackDescriptor([]byte("not gzip"))
	require.Error(t, err)
	assert.Nil(t, got)

	got, err = unpackDescriptor(gzipPayload(t, []byte{0xff}))
	require.Error(t, err)
	assert.Nil(t, got)
}

func TestGetOutputInputNames(t *testing.T) {
	input, output, err := getOutputInputNames(map[string]interface{}{
		"input":  map[string]interface{}{"*fixture.Input": map[string]interface{}{}},
		"output": []interface{}{map[string]interface{}{"*fixture.Output": map[string]interface{}{}}},
	})
	require.NoError(t, err)
	assert.Equal(t, "fixture.Input", input)
	assert.Equal(t, "fixture.Output", output)

	input, output, err = getOutputInputNames(map[string]interface{}{
		"input":  map[string]interface{}{"plain": true},
		"output": []interface{}{},
	})
	require.NoError(t, err)
	assert.Empty(t, input)
	assert.Empty(t, output)
}

func TestGetKeysRecursesThroughMapsAndSlices(t *testing.T) {
	keys := make(map[string]bool)
	getKeys(map[string]interface{}{
		"*fixture.Input": map[string]interface{}{
			"nested": map[string]interface{}{"*fixture.Output": true},
		},
		"items": []interface{}{
			map[string]interface{}{"*fixture.Nested": true},
			"ignored",
		},
	}, keys)

	assert.Equal(t, map[string]bool{
		"fixture.Input": true, "fixture.Output": true, "fixture.Nested": true,
	}, keys)
}

func TestBuildProtosFromDescriptor(t *testing.T) {
	pc := newProto()
	pc.info.Commands["fixture.echo"] = &Command{input: "fixture.Input", output: "fixture.Output"}

	require.NoError(t, pc.buildProtosFromDescriptor([]protoreflect.FileDescriptor{testDescriptor(t)}))
	command := pc.info.Commands["fixture.echo"]
	require.NotNil(t, command.inputMsgDescriptor)
	require.NotNil(t, command.outputMsgDescriptor)
	assert.Equal(t, protoreflect.FullName("fixture.Input"), command.inputMsgDescriptor.FullName())
	assert.Equal(t, protoreflect.FullName("fixture.Output"), command.outputMsgDescriptor.FullName())
	assert.True(t, pc.descriptorsNames["fixture.Input"])
	assert.True(t, pc.descriptorsNames["fixture.Output"])
}

func TestProtoClientConstructorsAndInformation(t *testing.T) {
	pc := NewProto("sys.docs", 250*time.Millisecond)
	assert.Equal(t, "sys.docs", pc.docsRoute)
	assert.Empty(t, pc.descriptorsRoute)
	assert.Equal(t, 250*time.Millisecond, pc.requestTimeout)
	assert.Equal(t, pc.IncomingMsgChan, pc.MsgChannel())
	assert.Nil(t, pc.ExportInformation())
	require.Error(t, pc.LoadInfo(nil))

	pc.AddPushResponse("fixture.push", "fixture.Output")
	pc.AddPushResponse("", "fixture.Output")
	pc.AddPushResponse("ignored", "")
	require.Len(t, pc.info.Commands, 1)
	assert.Equal(t, "fixture.Output", pc.info.Commands["fixture.push"].output)

	info := &ProtoBufferInfo{Commands: map[string]*Command{
		"fixture.echo": {input: "fixture.Input", output: "fixture.Output"},
	}}
	require.NoError(t, pc.LoadInfo(info))
	assert.Same(t, pc.ExportInformation(), &pc.info)
	assert.Equal(t, info.Commands, pc.ExportInformation().Commands)

	withDescriptor := NewWithDescriptor("sys.descriptors", "sys.docs", time.Second)
	assert.Equal(t, "sys.descriptors", withDescriptor.descriptorsRoute)
	assert.Equal(t, "sys.docs", withDescriptor.docsRoute)
	assert.Equal(t, time.Second, withDescriptor.requestTimeout)
}

func TestGetDescriptorsBuildsCommands(t *testing.T) {
	ctrl := gomock.NewController(t)
	conn := connmocks.NewMockPlayerConn(ctrl)
	pc := newProto()
	pc.conn = conn
	pc.descriptorsRoute = "sys.descriptors"
	pc.Client.IncomingMsgChan = make(chan *message.Message, 1)
	pc.AddPushResponse("fixture.push", "fixture.Output")

	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().Write(gomock.Any()).DoAndReturn(func(frame []byte) (int, error) {
		msg := decodeWireMessage(t, frame)
		assert.Equal(t, message.Request, msg.Type)
		assert.Equal(t, "sys.descriptors", msg.Route)
		names := &protos.ProtoNames{}
		require.NoError(t, proto.Unmarshal(msg.Data, names))
		assert.ElementsMatch(t, []string{"fixture.Input", "fixture.Output"}, names.Name)
		return len(frame), nil
	})

	descriptorsData, err := proto.Marshal(&protos.ProtoDescriptors{Desc: []*descriptorpb.FileDescriptorProto{testDescriptorProto()}})
	require.NoError(t, err)
	pc.Client.IncomingMsgChan <- &message.Message{Type: message.Response, Data: descriptorsData}

	docs, err := json.Marshal(map[string]interface{}{
		"handlers": map[string]interface{}{
			"fixture.echo": map[string]interface{}{
				"input":  map[string]interface{}{"*fixture.Input": map[string]interface{}{}},
				"output": []interface{}{map[string]interface{}{"*fixture.Output": map[string]interface{}{}}},
			},
		},
		"remotes": map[string]interface{}{
			"fixture.notify": map[string]interface{}{
				"input":  map[string]interface{}{"*fixture.Input": map[string]interface{}{}},
				"output": []interface{}{},
			},
		},
	})
	require.NoError(t, err)

	require.NoError(t, pc.getDescriptors(string(docs)))
	assert.Equal(t, "fixture.Input", pc.info.Commands["fixture.echo"].input)
	assert.Equal(t, "fixture.Output", pc.info.Commands["fixture.echo"].output)
	assert.Equal(t, "fixture.Input", pc.info.Commands["fixture.notify"].input)
	assert.NotNil(t, pc.info.Commands["fixture.echo"].inputMsgDescriptor)
	assert.NotNil(t, pc.info.Commands["fixture.echo"].outputMsgDescriptor)
	assert.NotNil(t, pc.info.Commands["fixture.push"].outputMsgDescriptor)

	require.Error(t, pc.getDescriptors("not json"))
}

func TestProtoClientRequestAndNotifySerialization(t *testing.T) {
	ctrl := gomock.NewController(t)
	conn := connmocks.NewMockPlayerConn(ctrl)
	descriptor := testDescriptor(t)
	inputDescriptor := descriptor.Messages().ByName("Input")
	outputDescriptor := descriptor.Messages().ByName("Output")
	pc := newProto()
	pc.conn = conn
	pc.ready = true
	pc.info.Commands["fixture.echo"] = &Command{
		input: "fixture.Input", output: "fixture.Output",
		inputMsgDescriptor: inputDescriptor, outputMsgDescriptor: outputDescriptor,
	}

	writes := 0
	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil).Times(2)
	conn.EXPECT().Write(gomock.Any()).DoAndReturn(func(frame []byte) (int, error) {
		msg := decodeWireMessage(t, frame)
		decoded := dynamicpb.NewMessage(inputDescriptor)
		require.NoError(t, proto.Unmarshal(msg.Data, decoded))
		jsonData, err := protojson.Marshal(decoded)
		require.NoError(t, err)
		assert.JSONEq(t, `{"value":"hello"}`, string(jsonData))
		if writes == 0 {
			assert.Equal(t, message.Request, msg.Type)
		} else {
			assert.Equal(t, message.Notify, msg.Type)
		}
		writes++
		return len(frame), nil
	}).Times(2)

	id, err := pc.SendRequest("fixture.echo", []byte(`{"value":"hello"}`))
	require.NoError(t, err)
	assert.Equal(t, uint(1), id)
	assert.Equal(t, outputDescriptor, pc.expectedInputDescriptor)
	require.NoError(t, pc.SendNotify("fixture.echo", []byte(`{"value":"hello"}`)))
	assert.Equal(t, 2, writes)

	_, err = pc.SendRequest("missing.route", nil)
	assert.EqualError(t, err, "Invalid Route: missing.route")
	_, err = pc.SendRequest("fixture.echo", []byte(`{"unknown":true}`))
	require.Error(t, err)
	assert.EqualError(t, pc.SendNotify("missing.route", nil), "invalid route")
	require.Error(t, pc.SendNotify("fixture.echo", []byte(`{"unknown":true}`)))
}

func TestProtoClientRequestWithoutInputDescriptor(t *testing.T) {
	ctrl := gomock.NewController(t)
	conn := connmocks.NewMockPlayerConn(ctrl)
	pc := newProto()
	pc.conn = conn
	pc.ready = true
	outputDescriptor := testDescriptor(t).Messages().ByName("Output")
	pc.info.Commands["fixture.noinput"] = &Command{outputMsgDescriptor: outputDescriptor}

	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().Write(gomock.Any()).DoAndReturn(func(frame []byte) (int, error) {
		msg := decodeWireMessage(t, frame)
		assert.Empty(t, msg.Data)
		return len(frame), nil
	})

	_, err := pc.SendRequest("fixture.noinput", []byte(`{"ignored":true}`))
	require.NoError(t, err)
	assert.Equal(t, outputDescriptor, pc.expectedInputDescriptor)
}

func TestProtoClientWaitForData(t *testing.T) {
	descriptor := testDescriptor(t)
	outputDescriptor := descriptor.Messages().ByName("Output")
	pc := newProto()
	pc.expectedInputDescriptor = outputDescriptor
	pc.info.Commands["fixture.echo"] = &Command{outputMsgDescriptor: outputDescriptor}
	pc.Client.IncomingMsgChan = make(chan *message.Message, 1)

	output := dynamicpb.NewMessage(outputDescriptor)
	output.Set(outputDescriptor.Fields().ByName("result"), protoreflect.ValueOfString("ok"))
	encoded, err := proto.Marshal(output)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		pc.waitForData()
		close(done)
	}()
	pc.Client.IncomingMsgChan <- &message.Message{Type: message.Response, Route: "fixture.echo", Data: encoded}

	select {
	case response := <-pc.IncomingMsgChan:
		assert.JSONEq(t, `{"result":"ok"}`, string(response.Data))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decoded proto response")
	}
	pc.closeChan <- true
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proto response loop did not stop")
	}
}

func TestClientBasicsAndHandshake(t *testing.T) {
	c := New(125 * time.Millisecond)
	assert.False(t, c.ConnectedStatus())
	assert.Equal(t, 125*time.Millisecond, c.requestTimeout)
	channel := make(chan *message.Message, 1)
	c.IncomingMsgChan = channel
	assert.Equal(t, channel, c.MsgChannel())

	handshake := &session.HandshakeData{User: map[string]interface{}{"token": "abc"}}
	c.SetClientHandshakeData(handshake)
	assert.Same(t, handshake, c.clientHandshakeData)

	ctrl := gomock.NewController(t)
	conn := connmocks.NewMockPlayerConn(ctrl)
	c.conn = conn
	conn.EXPECT().SetWriteDeadline(gomock.Any()).Return(nil)
	conn.EXPECT().Write(gomock.Any()).DoAndReturn(func(frame []byte) (int, error) {
		packets, err := codec.NewPomeloPacketDecoder().Decode(frame)
		require.NoError(t, err)
		require.Len(t, packets, 1)
		assert.Equal(t, packet.Type(packet.Handshake), packets[0].Type)
		var got session.HandshakeData
		require.NoError(t, json.Unmarshal(packets[0].Data, &got))
		assert.Equal(t, "abc", got.User["token"])
		return len(frame), nil
	})
	require.NoError(t, c.sendHandshakeRequest())

	assert.EqualError(t, c.ConnectTo("unsupported://localhost"), "unSupport schme:unsupported")
}

func TestReceiveHandshakeResponseAndDisconnect(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	c := New()
	c.conn = clientConn

	payload, err := json.Marshal(HandshakeData{Code: 200, Sys: HandshakeSys{Heartbeat: 15}})
	require.NoError(t, err)
	frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Handshake, payload)
	require.NoError(t, err)
	serverDone := make(chan error, 1)
	go func() {
		if _, writeErr := serverConn.Write(frame); writeErr != nil {
			serverDone <- writeErr
			return
		}
		header := make([]byte, codec.HeadLength)
		if _, readErr := io.ReadFull(serverConn, header); readErr != nil {
			serverDone <- readErr
			return
		}
		if packet.Type(header[0]) != packet.HandshakeAck {
			serverDone <- errors.New("client did not send handshake ack")
			return
		}
		serverDone <- nil
	}()

	heartbeat, err := c.receiveHandshakeResponse()
	require.NoError(t, err)
	assert.Equal(t, 15, heartbeat)
	require.NoError(t, <-serverDone)

	disconnected := make(chan CloseReason, 1)
	c.closeChan = make(chan struct{})
	c.Connected = true
	c.OnDisconnected(func(reason CloseReason) { disconnected <- reason })
	c.Disconnect(CloseReasonManual)
	assert.False(t, c.ConnectedStatus())
	assert.Equal(t, CloseReasonManual, <-disconnected)
	select {
	case <-c.closeChan:
	default:
		t.Fatal("disconnect did not close the client channel")
	}
	assert.False(t, c.Connected)
	c.Disconnect(CloseReasonError)
}
