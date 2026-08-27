package client

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestProtoClientAttemptAndCommitGuardBranches(t *testing.T) {
	pc := newProto()
	attempt, err := pc.beginConnectAttempt()
	require.NoError(t, err)
	_, err = pc.beginConnectAttempt()
	require.ErrorIs(t, err, errClientConnecting)
	assert.False(t, pc.connectAttemptCancelled(attempt))

	bootstrap := newProto()
	other := &protoConnectAttempt{done: make(chan struct{})}
	require.ErrorIs(t, pc.setBootstrapClient(other, &bootstrap.Client), errClientConnectionClosed)
	require.NoError(t, pc.setBootstrapClient(attempt, &bootstrap.Client))
	otherBootstrap := New()
	pc.clearBootstrapClient(attempt, otherBootstrap)
	assert.Same(t, &bootstrap.Client, attempt.bootstrap)
	pc.clearBootstrapClient(attempt, &bootstrap.Client)
	assert.Nil(t, attempt.bootstrap)

	pc.stateMu.Lock()
	attempt.cancelled = true
	pc.stateMu.Unlock()
	assert.True(t, pc.connectAttemptCancelled(attempt))
	require.ErrorIs(t, pc.setBootstrapClient(attempt, &bootstrap.Client), errClientConnectionClosed)
	require.ErrorIs(t, pc.commitBootstrap(attempt, bootstrap), errClientConnectionClosed)
	assert.True(t, pc.finishConnectAttempt(attempt))
	select {
	case <-attempt.done:
	default:
		t.Fatal("finished proto attempt did not close done")
	}

	assert.Nil(t, cloneCommand(nil))
	command := &Command{input: "fixture.Input", output: "fixture.Output"}
	cloned := cloneCommand(command)
	require.NotSame(t, command, cloned)
	assert.Equal(t, command.input, cloned.input)
	assert.Equal(t, command.output, cloned.output)

	pc = newProto()
	attempt, err = pc.beginConnectAttempt()
	require.NoError(t, err)
	bootstrap = newProto()
	bootstrap.info.Commands["fixture.echo"] = command
	bootstrap.descriptorsRoute = "sys.descriptors"
	bootstrap.descriptorsNames["fixture.Input"] = true
	require.ErrorIs(t, pc.commitBootstrap(other, bootstrap), errClientConnectionClosed)
	require.NoError(t, pc.commitBootstrap(attempt, bootstrap))
	assert.True(t, pc.ready)
	assert.Equal(t, "sys.descriptors", pc.descriptorsRoute)
	assert.Same(t, command, pc.info.Commands["fixture.echo"])
	assert.False(t, pc.finishConnectAttempt(attempt))
}

func TestProtoClientLoadAndDisconnectBranches(t *testing.T) {
	t.Run("load rejects concurrent attempt", func(t *testing.T) {
		pc := newProto()
		pc.activeAttempt = &protoConnectAttempt{done: make(chan struct{})}
		err := pc.LoadServerInfo("tcp://localhost:1")
		require.ErrorIs(t, err, errClientConnecting)
	})

	t.Run("load reports bootstrap dial error", func(t *testing.T) {
		pc := newProto()
		err := pc.LoadServerInfo("unsupported://localhost")
		require.EqualError(t, err, "unSupport schme:unsupported")
		assert.Nil(t, pc.activeAttempt)
		assert.False(t, pc.ready)
	})

	t.Run("load commits bootstrap", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()
		var accepts atomic.Int64
		serverDone := startProtoServerSequence(listener, []protoServerStep{{bootstrap: true}}, &accepts)

		pc := NewWithDescriptor("sys.descriptors", "sys.docs")
		require.NoError(t, pc.LoadServerInfo(listener.Addr().String()))
		assert.True(t, pc.ready)
		assert.NotNil(t, pc.info.Commands["fixture.echo"])
		select {
		case serverErr := <-serverDone:
			require.NoError(t, serverErr)
		case <-time.After(time.Second):
			t.Fatal("load-info server did not stop")
		}
	})

	t.Run("disconnect cancels bootstrap", func(t *testing.T) {
		pc := newProto()
		done := make(chan struct{})
		close(done)
		attempt := &protoConnectAttempt{done: done, bootstrap: New()}
		pc.activeAttempt = attempt
		pc.Disconnect(CloseReasonManual)
		assert.True(t, attempt.cancelled)
	})
}

func TestProtoClientWaitForDataErrorAndCloseBranches(t *testing.T) {
	descriptor := testDescriptor(t)
	outputDescriptor := descriptor.Messages().ByName("Output")
	pc := newProto()
	pc.info.Commands["fixture.echo"] = &Command{outputMsgDescriptor: outputDescriptor}
	gen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	reader := pc.startGenerationReader(gen)

	gen.incomingChan <- nil
	gen.incomingChan <- &message.Message{Type: message.Response, Err: true, Data: []byte{0xff}}
	statusData, err := proto.Marshal(&apierrors.Status{})
	require.NoError(t, err)
	gen.incomingChan <- &message.Message{Type: message.Response, Err: true, Data: statusData}

	select {
	case response := <-pc.IncomingMsgChan:
		assert.True(t, response.Err)
		assert.True(t, json.Valid(response.Data))
	case <-time.After(time.Second):
		t.Fatal("valid proto error was not delivered")
	}

	gen.incomingChan <- &message.Message{Type: message.Response, Route: "missing.route", Data: []byte("ignored")}
	gen.incomingChan <- &message.Message{Type: message.Response, Route: "fixture.echo", Data: []byte{0xff}}
	output := dynamicpb.NewMessage(outputDescriptor)
	output.Set(outputDescriptor.Fields().ByName("result"), protoreflect.ValueOfString("ok"))
	encoded, err := proto.Marshal(output)
	require.NoError(t, err)
	gen.incomingChan <- &message.Message{Type: message.Response, Route: "fixture.echo", Data: encoded}

	select {
	case response := <-pc.IncomingMsgChan:
		assert.JSONEq(t, `{"result":"ok"}`, string(response.Data))
	case <-time.After(time.Second):
		t.Fatal("valid proto response was not delivered after ignored inputs")
	}

	close(gen.closeCh)
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("proto reader did not stop")
	}
}

func TestProtoClientMessageAndDescriptorGuardBranches(t *testing.T) {
	pc := newProto()

	response, err := pc.receiveClientMessage(nil)
	assert.Nil(t, response)
	require.ErrorIs(t, err, errClientConnectionClosed)

	pc.Client.IncomingMsgChan = make(chan *message.Message, 1)
	pc.Client.IncomingMsgChan <- nil
	response, err = pc.receiveClientMessage(nil)
	assert.Nil(t, response)
	require.ErrorIs(t, err, errClientConnectionClosed)

	want := &message.Message{Type: message.Push, Route: "fixture.push"}
	pc.Client.IncomingMsgChan <- want
	response, err = pc.receiveClientMessage(nil)
	require.NoError(t, err)
	assert.Same(t, want, response)

	closedGen := newConnectionGeneration(1, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	close(closedGen.closeCh)
	response, err = pc.receiveClientMessage(closedGen)
	assert.Nil(t, response)
	require.ErrorIs(t, err, errClientConnectionClosed)

	nilGen := newConnectionGeneration(2, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	nilGen.incomingChan <- nil
	response, err = pc.receiveClientMessage(nilGen)
	assert.Nil(t, response)
	require.ErrorIs(t, err, errClientConnectionClosed)

	messageGen := newConnectionGeneration(3, newGenerationTestConn(), codec.NewPomeloPacketEncoder(), message.NewMessagesEncoder(false))
	messageGen.incomingChan <- want
	response, err = pc.receiveClientMessage(messageGen)
	require.NoError(t, err)
	assert.Same(t, want, response)

	assert.False(t, pc.deliverProtoMessage(messageGen, want))
	pc.reader = &protoGenerationReader{generation: closedGen, done: make(chan struct{})}
	pc.IncomingMsgChan = make(chan *message.Message)
	assert.False(t, pc.deliverProtoMessage(closedGen, want))

	outputDescriptor := testDescriptor(t).Messages().ByName("Output")
	assert.Nil(t, pc.outputDescriptorForResponse(messageGen, "fixture.echo"))
	pc.reader = &protoGenerationReader{generation: messageGen, done: make(chan struct{})}
	pc.info.Commands["fixture.echo"] = &Command{outputMsgDescriptor: outputDescriptor}
	assert.Equal(t, outputDescriptor, pc.outputDescriptorForResponse(messageGen, "fixture.echo"))
	pc.expectedInputDescriptor = outputDescriptor
	assert.Equal(t, outputDescriptor, pc.outputDescriptorForResponse(messageGen, "missing.route"))
	assert.Nil(t, pc.expectedInputDescriptor)
}

func TestProtoClientNotifyWithoutInputDescriptor(t *testing.T) {
	pc := newProto()
	pc.ready = true
	pc.info.Commands["fixture.notify"] = &Command{}
	conn := newGenerationTestConn()
	written := make(chan *message.Message, 1)
	conn.write = func(frame []byte) (int, error) {
		written <- decodeWireMessage(t, frame)
		return len(frame), nil
	}
	installTestGeneration(&pc.Client, conn, false, 0)

	require.NoError(t, pc.SendNotify("fixture.notify", []byte(`{"ignored":true}`)))
	select {
	case sent := <-written:
		assert.Equal(t, message.Notify, sent.Type)
		assert.Empty(t, sent.Data)
	case <-time.After(time.Second):
		t.Fatal("notify was not written")
	}
	pc.Disconnect(CloseReasonManual)
}

func startGatedHandshakeServer(t *testing.T) (net.Listener, <-chan struct{}, chan<- struct{}, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	requestRead := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		request, readErr := readPacket(conn)
		if readErr != nil {
			done <- readErr
			return
		}
		if request.Type != packet.Handshake {
			done <- errors.New("expected handshake")
			return
		}
		close(requestRead)
		<-release
		done <- serveHandshakeResponseOnly(conn, 0)
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, requestRead, release, done
}

func startGatedDescriptorServer(t *testing.T) (net.Listener, <-chan struct{}, chan<- struct{}, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	descriptorRead := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		if serveErr := serveClientHandshake(conn); serveErr != nil {
			done <- serveErr
			return
		}
		docsRequest, readErr := readProtoServerMessage(conn)
		if readErr != nil {
			done <- readErr
			return
		}
		docsData, marshalErr := proto.Marshal(&protos.Doc{Doc: protoDocsFixture()})
		if marshalErr != nil {
			done <- marshalErr
			return
		}
		if writeErr := writeProtoServerMessage(conn, &message.Message{Type: message.Response, ID: docsRequest.ID, Data: docsData}); writeErr != nil {
			done <- writeErr
			return
		}
		descriptorRequest, readErr := readProtoServerMessage(conn)
		if readErr != nil {
			done <- readErr
			return
		}
		close(descriptorRead)
		<-release
		descriptorData, marshalErr := proto.Marshal(&protos.ProtoDescriptors{Desc: []*descriptorpb.FileDescriptorProto{testDescriptorProto()}})
		if marshalErr == nil {
			marshalErr = writeProtoServerMessage(conn, &message.Message{Type: message.Response, ID: descriptorRequest.ID, Data: descriptorData})
		}
		if marshalErr != nil {
			done <- marshalErr
			return
		}
		_, readErr = io.Copy(io.Discard, conn)
		if readErr != nil && !errors.Is(readErr, net.ErrClosed) {
			done <- readErr
			return
		}
		done <- nil
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, descriptorRead, release, done
}

func TestProtoClientBootstrapCancellationBranches(t *testing.T) {
	t.Run("canceled after handshake", func(t *testing.T) {
		listener, requestRead, release, serverDone := startGatedHandshakeServer(t)
		pc := NewWithDescriptor("sys.descriptors", "sys.docs")
		attempt, err := pc.beginConnectAttempt()
		require.NoError(t, err)
		result := make(chan error, 1)
		go func() {
			_, bootstrapErr := pc.bootstrapServerInfo(attempt, listener.Addr().String(), nil)
			result <- bootstrapErr
		}()
		select {
		case <-requestRead:
		case <-time.After(time.Second):
			t.Fatal("bootstrap handshake request was not read")
		}
		pc.stateMu.Lock()
		attempt.cancelled = true
		pc.stateMu.Unlock()
		close(release)
		require.ErrorIs(t, <-result, errClientConnectionClosed)
		assert.True(t, pc.finishConnectAttempt(attempt))
		require.NoError(t, <-serverDone)
	})

	t.Run("canceled after descriptors", func(t *testing.T) {
		listener, descriptorRead, release, serverDone := startGatedDescriptorServer(t)
		pc := NewWithDescriptor("sys.descriptors", "sys.docs")
		attempt, err := pc.beginConnectAttempt()
		require.NoError(t, err)
		result := make(chan error, 1)
		go func() {
			_, bootstrapErr := pc.bootstrapServerInfo(attempt, listener.Addr().String(), nil)
			result <- bootstrapErr
		}()
		select {
		case <-descriptorRead:
		case <-time.After(time.Second):
			t.Fatal("descriptor request was not read")
		}
		pc.stateMu.Lock()
		attempt.cancelled = true
		pc.stateMu.Unlock()
		close(release)
		require.ErrorIs(t, <-result, errClientConnectionClosed)
		assert.True(t, pc.finishConnectAttempt(attempt))
		select {
		case serverErr := <-serverDone:
			require.NoError(t, serverErr)
		case <-time.After(time.Second):
			t.Fatal("descriptor server did not stop")
		}
	})

	pc := newProto()
	_, err := pc.bootstrapServerInfo(&protoConnectAttempt{done: make(chan struct{})}, "tcp://localhost:1", nil)
	require.ErrorIs(t, err, errClientConnectionClosed)
}

func TestProtoClientBootstrapResponseErrors(t *testing.T) {
	tests := []struct {
		name       string
		response   []byte
		wantErr    string
		noResponse bool
	}{
		{name: "connection closes before docs", wantErr: "failed to receive docs route response", noResponse: true},
		{name: "invalid docs protobuf", response: []byte{0xff}, wantErr: "failed to unmarshal docs route response"},
		{name: "invalid docs payload", wantErr: "failed to read proto descriptors"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			serverDone := make(chan error, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					serverDone <- acceptErr
					return
				}
				defer conn.Close()
				if serveErr := serveClientHandshake(conn); serveErr != nil {
					serverDone <- serveErr
					return
				}
				docsRequest, readErr := readProtoServerMessage(conn)
				if readErr != nil {
					serverDone <- readErr
					return
				}
				if test.noResponse {
					serverDone <- nil
					return
				}
				responseData := test.response
				if responseData == nil {
					responseData, readErr = proto.Marshal(&protos.Doc{Doc: "not json"})
					if readErr != nil {
						serverDone <- readErr
						return
					}
				}
				if writeErr := writeProtoServerMessage(conn, &message.Message{Type: message.Response, ID: docsRequest.ID, Data: responseData}); writeErr != nil {
					serverDone <- writeErr
					return
				}
				_, readErr = io.Copy(io.Discard, conn)
				if readErr != nil && !errors.Is(readErr, net.ErrClosed) {
					serverDone <- readErr
					return
				}
				serverDone <- nil
			}()

			pc := NewWithDescriptor("sys.descriptors", "sys.docs")
			attempt, err := pc.beginConnectAttempt()
			require.NoError(t, err)
			_, err = pc.bootstrapServerInfo(attempt, listener.Addr().String(), nil)
			require.ErrorContains(t, err, test.wantErr)
			pc.finishConnectAttempt(attempt)
			select {
			case serverErr := <-serverDone:
				require.NoError(t, serverErr)
			case <-time.After(time.Second):
				t.Fatal("bootstrap error server did not stop")
			}
		})
	}
}

func TestProtoClientConnectFinalDialError(t *testing.T) {
	pc := newProto()
	pc.ready = true
	err := pc.ConnectTo("unsupported://localhost")
	require.EqualError(t, err, "unSupport schme:unsupported")
	assert.Nil(t, pc.activeAttempt)

	done := make(chan struct{})
	pc.activeAttempt = &protoConnectAttempt{done: done}
	err = pc.ConnectTo("tcp://localhost:1")
	require.ErrorIs(t, err, errClientConnecting)
	close(done)
}
