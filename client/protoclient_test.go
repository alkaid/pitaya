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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
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
			{
				Name: proto.String("CountOutput"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("count"),
						JsonName: proto.String("count"),
						Number:   proto.Int32(1),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
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

func writeProtoServerMessage(conn net.Conn, msg *message.Message) error {
	encoded, err := message.NewMessagesEncoder(false).Encode(msg)
	if err != nil {
		return err
	}
	frame, err := codec.NewPomeloPacketEncoder().Encode(packet.Data, encoded)
	if err != nil {
		return err
	}
	_, err = conn.Write(frame)
	return err
}

func readProtoServerMessage(conn net.Conn) (*message.Message, error) {
	p, err := readPacket(conn)
	if err != nil {
		return nil, err
	}
	if p.Type != packet.Data {
		return nil, fmt.Errorf("packet type = %v, want data", p.Type)
	}
	return message.Decode(p.Data)
}

func protoDocsFixture() string {
	return `{"handlers":{"fixture.echo":{"input":{"*fixture.Input":{}},"output":[{"*fixture.Output":{}}]}},"remotes":{}}`
}

func serveProtoBootstrap(conn net.Conn, malformedDocs bool) error {
	if err := serveClientHandshake(conn); err != nil {
		return err
	}
	docsRequest, err := readProtoServerMessage(conn)
	if err != nil {
		return fmt.Errorf("read docs request: %w", err)
	}
	if docsRequest.Route != "sys.docs" {
		return fmt.Errorf("docs route = %q", docsRequest.Route)
	}
	docsData := []byte{0xff}
	if !malformedDocs {
		docsData, err = proto.Marshal(&protos.Doc{Doc: protoDocsFixture()})
		if err != nil {
			return err
		}
	}
	if err := writeProtoServerMessage(conn, &message.Message{
		Type: message.Response,
		ID:   docsRequest.ID,
		Data: docsData,
	}); err != nil {
		return fmt.Errorf("write docs response: %w", err)
	}
	if malformedDocs {
		return nil
	}

	descriptorRequest, err := readProtoServerMessage(conn)
	if err != nil {
		return fmt.Errorf("read descriptor request: %w", err)
	}
	if descriptorRequest.Route != "sys.descriptors" {
		return fmt.Errorf("descriptor route = %q", descriptorRequest.Route)
	}
	descriptorData, err := proto.Marshal(&protos.ProtoDescriptors{
		Desc: []*descriptorpb.FileDescriptorProto{testDescriptorProto()},
	})
	if err != nil {
		return err
	}
	if err := writeProtoServerMessage(conn, &message.Message{
		Type: message.Response,
		ID:   descriptorRequest.ID,
		Data: descriptorData,
	}); err != nil {
		return fmt.Errorf("write descriptor response: %w", err)
	}
	_, err = readPacket(conn)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

type protoServerStep struct {
	bootstrap    bool
	malformed    bool
	ready        chan struct{}
	release      <-chan struct{}
	remoteClose  bool
	pushAfterAck bool
}

func startProtoServerSequence(
	listener net.Listener,
	steps []protoServerStep,
	accepts *atomic.Int64,
) <-chan error {
	done := make(chan error, 1)
	go func() {
		for _, step := range steps {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			accepts.Add(1)
			if step.bootstrap {
				err = serveProtoBootstrap(conn, step.malformed)
				_ = conn.Close()
				if err != nil {
					done <- err
					return
				}
				continue
			}
			if err = serveClientHandshake(conn); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if step.ready != nil {
				close(step.ready)
			}
			if step.release != nil {
				<-step.release
			}
			if step.pushAfterAck {
				descriptor, descriptorErr := protodesc.NewFile(testDescriptorProto(), nil)
				if descriptorErr != nil {
					_ = conn.Close()
					done <- descriptorErr
					return
				}
				outputDescriptor := descriptor.Messages().ByName("Output")
				output := dynamicpb.NewMessage(outputDescriptor)
				output.Set(outputDescriptor.Fields().ByName("result"), protoreflect.ValueOfString("new"))
				encoded, encodeErr := proto.Marshal(output)
				if encodeErr == nil {
					encodeErr = writeProtoServerMessage(conn, &message.Message{
						Type:  message.Push,
						Route: "fixture.push",
						Data:  encoded,
					})
				}
				if encodeErr != nil {
					_ = conn.Close()
					done <- encodeErr
					return
				}
			}
			if step.remoteClose {
				_ = conn.Close()
				continue
			}
			_, err = readPacket(conn)
			_ = conn.Close()
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return done
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
	gen := installTestGeneration(&pc.Client, conn, false, 0)
	pc.descriptorsRoute = "sys.descriptors"
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
	gen.incomingChan <- &message.Message{Type: message.Response, Data: descriptorsData}

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
	installTestGeneration(&pc.Client, conn, false, 0)
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
	installTestGeneration(&pc.Client, conn, false, 0)
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

func TestProtoClientConcurrentResponsesUseTheirRequestRoutes(t *testing.T) {
	descriptor := testDescriptor(t)
	stringOutput := descriptor.Messages().ByName("Output")
	countOutput := descriptor.Messages().ByName("CountOutput")
	pc := newProto()
	gen := installTestGeneration(&pc.Client, newGenerationTestConn(), false, 0)
	pc.ready = true
	pc.info.Commands["fixture.string"] = &Command{outputMsgDescriptor: stringOutput}
	pc.info.Commands["fixture.count"] = &Command{outputMsgDescriptor: countOutput}

	reader := pc.startGenerationReader(gen)
	packetsDone := make(chan struct{})
	go func() {
		defer close(packetsDone)
		pc.Client.handlePackets(gen)
	}()
	t.Cleanup(func() {
		pc.Disconnect(CloseReasonManual)
		<-reader.done
		<-packetsDone
	})

	stringID, err := pc.SendRequest("fixture.string", nil)
	require.NoError(t, err)
	countID, err := pc.SendRequest("fixture.count", nil)
	require.NoError(t, err)

	stringMessage := dynamicpb.NewMessage(stringOutput)
	stringMessage.Set(stringOutput.Fields().ByName("result"), protoreflect.ValueOfString("first"))
	stringData, err := proto.Marshal(stringMessage)
	require.NoError(t, err)
	countMessage := dynamicpb.NewMessage(countOutput)
	countMessage.Set(countOutput.Fields().ByName("count"), protoreflect.ValueOfInt32(2))
	countData, err := proto.Marshal(countMessage)
	require.NoError(t, err)

	encodeResponse := func(id uint, data []byte) *packet.Packet {
		encoded, encodeErr := message.NewMessagesEncoder(false).Encode(&message.Message{
			Type: message.Response,
			ID:   id,
			Data: data,
		})
		require.NoError(t, encodeErr)
		return &packet.Packet{Type: packet.Data, Data: encoded}
	}
	gen.packetChan <- encodeResponse(stringID, stringData)
	gen.packetChan <- encodeResponse(countID, countData)

	receive := func() *message.Message {
		t.Helper()
		select {
		case response := <-pc.IncomingMsgChan:
			return response
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for decoded concurrent response")
			return nil
		}
	}
	first := receive()
	assert.Equal(t, "fixture.string", first.Route)
	assert.JSONEq(t, `{"result":"first"}`, string(first.Data))
	second := receive()
	assert.Equal(t, "fixture.count", second.Route)
	assert.JSONEq(t, `{"count":2}`, string(second.Data))
}

func TestProtoClientWaitForData(t *testing.T) {
	descriptor := testDescriptor(t)
	outputDescriptor := descriptor.Messages().ByName("Output")
	pc := newProto()
	pc.expectedInputDescriptor = outputDescriptor
	pc.info.Commands["fixture.echo"] = &Command{outputMsgDescriptor: outputDescriptor}
	gen := newConnectionGeneration(1, nil, codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))

	output := dynamicpb.NewMessage(outputDescriptor)
	output.Set(outputDescriptor.Fields().ByName("result"), protoreflect.ValueOfString("ok"))
	encoded, err := proto.Marshal(output)
	require.NoError(t, err)

	reader := pc.startGenerationReader(gen)
	gen.incomingChan <- &message.Message{Type: message.Response, Route: "fixture.echo", Data: encoded}

	select {
	case response := <-pc.IncomingMsgChan:
		assert.JSONEq(t, `{"result":"ok"}`, string(response.Data))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for decoded proto response")
	}
	close(gen.closeCh)
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("proto response loop did not stop")
	}
}

func TestProtoClientFreshConnectToPlainAndTLS(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T) (net.Listener, string, []*tls.Config)
	}{
		{
			name: "plain",
			setup: func(t *testing.T) (net.Listener, string, []*tls.Config) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				return listener, listener.Addr().String(), nil
			},
		},
		{
			name: "TLS",
			setup: func(t *testing.T) (net.Listener, string, []*tls.Config) {
				certificate, leaf := newTestServerCertificate(t)
				listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
					Certificates: []tls.Certificate{certificate},
					MinVersion:   tls.VersionTLS12,
				})
				require.NoError(t, err)
				config := &tls.Config{
					RootCAs:    rootPoolForCertificate(leaf),
					MinVersion: tls.VersionTLS12,
				}
				return listener, tlsURIForLocalhost(t, listener.Addr().String()), []*tls.Config{config}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener, uri, configs := test.setup(t)
			t.Cleanup(func() { _ = listener.Close() })
			var accepts atomic.Int64
			serverDone := startProtoServerSequence(listener, []protoServerStep{
				{bootstrap: true},
				{},
			}, &accepts)

			pc := NewWithDescriptor("sys.descriptors", "sys.docs")
			var callbacks atomic.Int64
			pc.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
			require.NoError(t, pc.ConnectTo(uri, configs...))
			assert.True(t, pc.ConnectedStatus())
			assert.Zero(t, callbacks.Load(), "bootstrap must not invoke the public callback")
			assert.Equal(t, int64(2), accepts.Load())

			pc.Disconnect(CloseReasonManual)
			select {
			case err := <-serverDone:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("proto server did not stop")
			}
			assert.Equal(t, int64(1), callbacks.Load())
		})
	}
}

func TestProtoClientBootstrapFailureLeavesIdleAndRetrySucceeds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var accepts atomic.Int64
	serverDone := startProtoServerSequence(listener, []protoServerStep{
		{bootstrap: true, malformed: true},
		{bootstrap: true},
		{},
	}, &accepts)

	pc := NewWithDescriptor("sys.descriptors", "sys.docs")
	var callbacks atomic.Int64
	pc.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	require.Error(t, pc.ConnectTo(listener.Addr().String()))
	assert.False(t, pc.ConnectedStatus())
	pc.stateMu.RLock()
	assert.False(t, pc.ready)
	pc.stateMu.RUnlock()
	assert.Zero(t, callbacks.Load())

	require.NoError(t, pc.ConnectTo(listener.Addr().String()))
	assert.True(t, pc.ConnectedStatus())
	assert.Equal(t, int64(3), accepts.Load())
	assert.Zero(t, callbacks.Load())
	pc.Disconnect(CloseReasonManual)
	select {
	case serverErr := <-serverDone:
		require.NoError(t, serverErr)
	case <-time.After(time.Second):
		t.Fatal("proto retry server did not stop")
	}
}

func TestProtoClientReaderStopsWithGenerationAndReadyReconnectSkipsBootstrap(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	finalReady := make(chan struct{})
	releaseFinal := make(chan struct{})
	var accepts atomic.Int64
	serverDone := startProtoServerSequence(listener, []protoServerStep{
		{bootstrap: true},
		{ready: finalReady, release: releaseFinal, remoteClose: true},
		{pushAfterAck: true},
	}, &accepts)

	pc := NewWithDescriptor("sys.descriptors", "sys.docs")
	pc.AddPushResponse("fixture.push", "fixture.Output")
	var callbacks atomic.Int64
	pc.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	require.NoError(t, pc.ConnectTo(listener.Addr().String()))
	select {
	case <-finalReady:
	case <-time.After(time.Second):
		t.Fatal("final connection handshake did not finish")
	}
	oldGen := pc.Client.currentGeneration()
	pc.stateMu.RLock()
	oldReader := pc.reader
	pc.stateMu.RUnlock()
	require.NotNil(t, oldGen)
	require.NotNil(t, oldReader)
	close(releaseFinal)

	waitForGenerationLoops(t, oldGen)
	select {
	case <-oldReader.done:
	case <-time.After(time.Second):
		t.Fatal("old proto reader did not stop")
	}
	assert.False(t, pc.ConnectedStatus())
	assert.Equal(t, int64(1), callbacks.Load())

	require.NoError(t, pc.ConnectTo(listener.Addr().String()))
	select {
	case response := <-pc.IncomingMsgChan:
		assert.Equal(t, "fixture.push", response.Route)
		assert.JSONEq(t, `{"result":"new"}`, string(response.Data))
	case <-time.After(time.Second):
		t.Fatal("reconnected generation did not deliver its push")
	}
	assert.Equal(t, int64(3), accepts.Load(), "ready reconnect must not download descriptors again")
	pc.Disconnect(CloseReasonManual)
	select {
	case serverErr := <-serverDone:
		require.NoError(t, serverErr)
	case <-time.After(time.Second):
		t.Fatal("proto reconnect server did not stop")
	}
}

func TestProtoClientReaderHandoffWaitsForPreviousReader(t *testing.T) {
	pc := newProto()
	oldGen := newConnectionGeneration(1, nil, codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	newGen := newConnectionGeneration(2, nil, codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	previousDone := make(chan struct{})
	previous := &protoGenerationReader{generation: oldGen, done: previousDone}
	pc.reader = previous

	started := make(chan struct{})
	replacementDone := make(chan *protoGenerationReader, 1)
	go func() {
		close(started)
		replacementDone <- pc.startGenerationReader(newGen)
	}()
	<-started

	var replacement *protoGenerationReader
	replacedEarly := false
	select {
	case replacement = <-replacementDone:
		replacedEarly = true
	case <-time.After(50 * time.Millisecond):
		pc.stateMu.RLock()
		assert.Same(t, previous, pc.reader)
		pc.stateMu.RUnlock()
	}

	close(previousDone)
	if replacement == nil {
		select {
		case replacement = <-replacementDone:
		case <-time.After(time.Second):
			t.Fatal("replacement reader did not start after previous reader stopped")
		}
	}
	require.NotNil(t, replacement)
	close(newGen.closeCh)
	select {
	case <-replacement.done:
	case <-time.After(time.Second):
		t.Fatal("replacement reader did not stop")
	}

	assert.False(t, replacedEarly, "replacement reader became current before the previous reader stopped")
	pc.stateMu.RLock()
	assert.Nil(t, pc.reader)
	pc.stateMu.RUnlock()
}

func TestProtoClientDisconnectCancelsReaderHandoff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	serverReady := make(chan struct{})
	var accepts atomic.Int64
	serverDone := startProtoServerSequence(listener, []protoServerStep{{ready: serverReady}}, &accepts)

	pc := newProto()
	pc.ready = true
	oldGen := newConnectionGeneration(1, nil, codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	previousDone := make(chan struct{})
	releasedPrevious := false
	t.Cleanup(func() {
		if !releasedPrevious {
			close(previousDone)
		}
	})
	previous := &protoGenerationReader{generation: oldGen, done: previousDone}
	pc.reader = previous

	connectResult := make(chan error, 1)
	go func() { connectResult <- pc.ConnectTo(listener.Addr().String()) }()
	select {
	case <-serverReady:
	case <-time.After(time.Second):
		t.Fatal("final connection handshake did not finish")
	}
	require.Eventually(t, func() bool {
		return pc.Client.currentGeneration() != nil
	}, time.Second, time.Millisecond)
	pc.stateMu.RLock()
	assert.Same(t, previous, pc.reader)
	pc.stateMu.RUnlock()

	disconnectDone := make(chan struct{})
	go func() {
		pc.Disconnect(CloseReasonManual)
		close(disconnectDone)
	}()
	require.Eventually(t, func() bool {
		return !pc.ConnectedStatus()
	}, time.Second, time.Millisecond)
	select {
	case <-disconnectDone:
		t.Fatal("Disconnect returned before the active connect attempt finished")
	default:
	}

	releasedPrevious = true
	close(previousDone)
	select {
	case connectErr := <-connectResult:
		assert.ErrorIs(t, connectErr, errClientConnectionClosed)
	case <-time.After(time.Second):
		t.Fatal("ConnectTo did not finish after the previous reader stopped")
	}
	select {
	case <-disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not join the canceled connect attempt")
	}
	select {
	case serverErr := <-serverDone:
		require.NoError(t, serverErr)
	case <-time.After(time.Second):
		t.Fatal("proto server did not stop")
	}
	assert.False(t, pc.ConnectedStatus())
	assert.Equal(t, int64(1), accepts.Load())
}

func TestProtoClientDisconnectCallbackCanReconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var accepts atomic.Int64
	serverDone := startProtoServerSequence(listener, []protoServerStep{
		{bootstrap: true},
		{remoteClose: true},
		{pushAfterAck: true},
	}, &accepts)

	pc := NewWithDescriptor("sys.descriptors", "sys.docs")
	pc.AddPushResponse("fixture.push", "fixture.Output")
	reconnectResult := make(chan error, 1)
	var callbacks atomic.Int64
	pc.OnDisconnected(func(CloseReason) {
		if callbacks.Add(1) == 1 {
			reconnectResult <- pc.ConnectTo(listener.Addr().String())
		}
	})
	require.NoError(t, pc.ConnectTo(listener.Addr().String()))

	select {
	case reconnectErr := <-reconnectResult:
		require.NoError(t, reconnectErr)
	case <-time.After(time.Second):
		t.Fatal("disconnect callback did not finish reconnecting")
	}
	select {
	case response := <-pc.IncomingMsgChan:
		assert.Equal(t, "fixture.push", response.Route)
		assert.JSONEq(t, `{"result":"new"}`, string(response.Data))
	case <-time.After(time.Second):
		t.Fatal("callback replacement did not deliver its push")
	}

	pc.Disconnect(CloseReasonManual)
	select {
	case serverErr := <-serverDone:
		require.NoError(t, serverErr)
	case <-time.After(time.Second):
		t.Fatal("callback reconnect server did not stop")
	}
	assert.Equal(t, int64(3), accepts.Load())
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
	gen := installTestGeneration(c, clientConn, false, 0)
	c.OnDisconnected(func(reason CloseReason) { disconnected <- reason })
	c.Disconnect(CloseReasonManual)
	assert.False(t, c.ConnectedStatus())
	assert.Equal(t, CloseReasonManual, <-disconnected)
	select {
	case <-gen.closeCh:
	default:
		t.Fatal("disconnect did not close the client channel")
	}
	assert.False(t, c.Connected)
	c.Disconnect(CloseReasonError)
}
