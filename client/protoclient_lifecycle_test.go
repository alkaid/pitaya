package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
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
		case response := <-pc.MsgChannel():
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
	case response := <-pc.MsgChannel():
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

func TestProtoClientDisconnectCallbackCanReconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var accepts atomic.Int64
	release := make(chan struct{})
	serverDone := startProtoServerSequence(listener, []protoServerStep{
		{bootstrap: true},
		{remoteClose: true, release: release},
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
	close(release)

	select {
	case reconnectErr := <-reconnectResult:
		require.NoError(t, reconnectErr)
	case <-time.After(time.Second):
		t.Fatal("disconnect callback did not finish reconnecting")
	}
	select {
	case response := <-pc.MsgChannel():
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

func newTestServerCertificate(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	require.NoError(t, err)

	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	certificate.Leaf = leaf
	return certificate, leaf
}

func rootPoolForCertificate(certificate *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return pool
}

func tlsURIForLocalhost(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	return "tls://" + net.JoinHostPort("localhost", port)
}

func serveClientHandshake(conn net.Conn) error {
	request, err := readPacket(conn)
	if err != nil {
		return fmt.Errorf("read handshake request: %w", err)
	}
	if request.Type != packet.Handshake {
		return fmt.Errorf("handshake request type = %v", request.Type)
	}
	payload, err := json.Marshal(HandshakeData{Code: http.StatusOK, Sys: HandshakeSys{Heartbeat: 0}})
	if err != nil {
		return err
	}
	response, err := codec.NewPomeloPacketEncoder().Encode(packet.Handshake, payload)
	if err != nil {
		return err
	}
	if _, err := conn.Write(response); err != nil {
		return fmt.Errorf("write handshake response: %w", err)
	}
	ack, err := readPacket(conn)
	if err != nil {
		return fmt.Errorf("read handshake ack: %w", err)
	}
	if ack.Type != packet.HandshakeAck {
		return fmt.Errorf("handshake ack type = %v", ack.Type)
	}
	return nil
}
