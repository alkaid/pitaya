package main

import (
	"crypto/tls"
	"errors"
	"io"
	stdlog "log"
	"reflect"
	"sync"
	"testing"

	"github.com/alkaid/pitaya/v2/client"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/session"
)

type recordingPitayaClient struct {
	mu       sync.Mutex
	attempts []string
	err      error
}

func (c *recordingPitayaClient) ConnectTo(address string, configs ...*tls.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts = append(c.attempts, address)
	if len(configs) != 0 {
		return errors.New("CLI supplied an unexpected TLS config")
	}
	return c.err
}

func (*recordingPitayaClient) ConnectedStatus() bool                          { return false }
func (*recordingPitayaClient) Disconnect(client.CloseReason)                  {}
func (*recordingPitayaClient) MsgChannel() chan *message.Message              { return nil }
func (*recordingPitayaClient) SendNotify(string, []byte) error                { return nil }
func (*recordingPitayaClient) SendRequest(string, []byte) (uint, error)       { return 0, nil }
func (*recordingPitayaClient) SetClientHandshakeData(*session.HandshakeData)  {}
func (*recordingPitayaClient) OnDisconnected(func(reason client.CloseReason)) {}

func (c *recordingPitayaClient) recordedAttempts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.attempts...)
}

func TestTryConnectUsesOnlyExplicitScheme(t *testing.T) {
	wantErr := errors.New("connect failed")
	recorder := &recordingPitayaClient{err: wantErr}
	previous := pClient
	pClient = recorder
	t.Cleanup(func() { pClient = previous })

	err := tryConnect("wss://example.test:3250")
	if !errors.Is(err, wantErr) {
		t.Fatalf("tryConnect error = %v, want %v", err, wantErr)
	}
	wantAttempts := []string{"wss://example.test:3250"}
	if got := recorder.recordedAttempts(); !reflect.DeepEqual(got, wantAttempts) {
		t.Fatalf("connection attempts = %v, want %v", got, wantAttempts)
	}
}

func TestTryConnectTreatsAddressWithoutSchemeAsPlainTCP(t *testing.T) {
	wantErr := errors.New("connect failed")
	recorder := &recordingPitayaClient{err: wantErr}
	previous := pClient
	pClient = recorder
	t.Cleanup(func() { pClient = previous })

	err := tryConnect("127.0.0.1:3250")
	if !errors.Is(err, wantErr) {
		t.Fatalf("tryConnect error = %v, want %v", err, wantErr)
	}
	wantAttempts := []string{"tcp://127.0.0.1:3250"}
	if got := recorder.recordedAttempts(); !reflect.DeepEqual(got, wantAttempts) {
		t.Fatalf("connection attempts = %v, want %v", got, wantAttempts)
	}
}

func TestProtoClientPreparationDefersServerInfoLoadToConnectTo(t *testing.T) {
	previousClient := pClient
	previousDocs := docsString
	previousPushInfo := pushInfo
	docsString = "sys.docs"
	pushInfo = map[string]string{"fixture.push": "fixture.Output"}
	t.Cleanup(func() {
		pClient = previousClient
		docsString = previousDocs
		pushInfo = previousPushInfo
	})

	logger := stdlog.New(io.Discard, "", 0)
	if err := protoClient(logger, "not-a-network-address"); err != nil {
		t.Fatalf("protoClient preparation returned error: %v", err)
	}
	prepared, ok := pClient.(*client.ProtoClient)
	if !ok {
		t.Fatalf("prepared client type = %T, want *client.ProtoClient", pClient)
	}
	if prepared.ConnectedStatus() {
		t.Fatal("protoClient preparation unexpectedly connected to the server")
	}
	if prepared.ExportInformation() != nil {
		t.Fatal("server information was loaded before ConnectTo")
	}
}
