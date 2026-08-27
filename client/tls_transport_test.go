package client

import (
	"bytes"
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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

type rawTLSTestServer struct {
	listener net.Listener
	done     chan error
	release  chan struct{}
	once     sync.Once
}

func startRawTLSTestServer(t *testing.T, certificate tls.Certificate) *rawTLSTestServer {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	require.NoError(t, err)
	server := &rawTLSTestServer{
		listener: listener,
		done:     make(chan error, 1),
		release:  make(chan struct{}),
	}
	t.Cleanup(func() {
		server.stop()
		_ = listener.Close()
	})

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			server.done <- acceptErr
			return
		}
		defer conn.Close()
		if handshakeErr := serveClientHandshake(conn); handshakeErr != nil {
			server.done <- handshakeErr
			return
		}
		<-server.release
		server.done <- nil
	}()
	return server
}

func (s *rawTLSTestServer) stop() {
	s.once.Do(func() { close(s.release) })
}

func tlsURIForLocalhost(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	return "tls://" + net.JoinHostPort("localhost", port)
}

func TestCloneTLSConfigRejectsNilAndDoesNotMutateCaller(t *testing.T) {
	_, err := cloneTLSConfig([]*tls.Config{nil})
	require.Error(t, err)

	original := &tls.Config{
		ServerName: "example.test",
		NextProtos: []string{"pitaya"},
		MinVersion: tls.VersionTLS12,
	}
	cloned, err := cloneTLSConfig([]*tls.Config{original})
	require.NoError(t, err)
	require.NotSame(t, original, cloned)
	cloned.ServerName = "changed.test"
	assert.Equal(t, "example.test", original.ServerName)
	assert.Equal(t, []string{"pitaya"}, original.NextProtos)
}

func TestConnectToTLSVerifiesIdentityWithoutMutatingCallerConfig(t *testing.T) {
	certificate, leaf := newTestServerCertificate(t)
	server := startRawTLSTestServer(t, certificate)
	config := &tls.Config{
		RootCAs:    rootPoolForCertificate(leaf),
		MinVersion: tls.VersionTLS12,
	}

	c := New()
	require.NoError(t, c.ConnectTo(tlsURIForLocalhost(t, server.listener.Addr().String()), config))
	assert.Empty(t, config.ServerName)
	assert.False(t, config.InsecureSkipVerify)
	assert.True(t, c.ConnectedStatus())

	c.Disconnect(CloseReasonManual)
	server.stop()
	require.NoError(t, <-server.done)
}

func TestConnectToTLSRejectsUnknownCAAndHostnameMismatch(t *testing.T) {
	certificate, leaf := newTestServerCertificate(t)
	tests := []struct {
		name   string
		config *tls.Config
	}{
		{name: "unknown CA", config: &tls.Config{MinVersion: tls.VersionTLS12}},
		{name: "hostname mismatch", config: &tls.Config{
			RootCAs:    rootPoolForCertificate(leaf),
			ServerName: "wrong.example",
			MinVersion: tls.VersionTLS12,
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startRawTLSTestServer(t, certificate)
			c := New()
			c.handshakeTimeout = time.Second
			err := c.ConnectTo(tlsURIForLocalhost(t, server.listener.Addr().String()), test.config)
			require.Error(t, err)
			assert.Nil(t, c.conn)
			server.stop()
			require.Error(t, <-server.done)
		})
	}
}

func TestConnectToTLSRespectsExplicitCallerInsecureOverride(t *testing.T) {
	certificate, _ := newTestServerCertificate(t)
	server := startRawTLSTestServer(t, certificate)
	config := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec -- this test verifies the explicit caller override
		MinVersion:         tls.VersionTLS12,
	}
	c := New()
	require.NoError(t, c.ConnectTo(tlsURIForLocalhost(t, server.listener.Addr().String()), config))
	assert.Empty(t, config.ServerName)
	assert.True(t, config.InsecureSkipVerify)

	c.Disconnect(CloseReasonManual)
	server.stop()
	require.NoError(t, <-server.done)
}

func TestConnectToStalledHandshakeTimesOutAndClearsConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		if _, readErr := readPacket(conn); readErr != nil {
			serverDone <- readErr
			return
		}
		var data [1]byte
		_, readErr := conn.Read(data[:])
		serverDone <- readErr
	}()

	var callbacks atomic.Int64
	c := New()
	c.handshakeTimeout = 100 * time.Millisecond
	c.OnDisconnected(func(CloseReason) { callbacks.Add(1) })
	started := time.Now()
	err = c.ConnectTo("tcp://" + listener.Addr().String())
	require.Error(t, err)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.Nil(t, c.conn)
	assert.False(t, c.ConnectedStatus())
	assert.Zero(t, callbacks.Load())
	serverErr := <-serverDone
	require.Error(t, serverErr)
	assert.True(t, errors.Is(serverErr, io.EOF) || errors.Is(serverErr, net.ErrClosed), serverErr)
}

func newWSSHandshakeServer(t *testing.T, expectedConnections int) (*httptest.Server, *x509.CertPool, <-chan error) {
	t.Helper()
	results := make(chan error, expectedConnections)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			results <- err
			return
		}
		defer conn.Close()
		_, requestFrame, err := conn.ReadMessage()
		if err != nil {
			results <- err
			return
		}
		request, err := readPacket(bytes.NewReader(requestFrame))
		if err != nil {
			results <- fmt.Errorf("decode websocket handshake request: %w", err)
			return
		}
		if request.Type != packet.Handshake {
			results <- fmt.Errorf("websocket handshake request type = %v", request.Type)
			return
		}
		payload, err := json.Marshal(HandshakeData{Code: http.StatusOK, Sys: HandshakeSys{Heartbeat: 0}})
		if err != nil {
			results <- err
			return
		}
		responseFrame, err := codec.NewPomeloPacketEncoder().Encode(packet.Handshake, payload)
		if err == nil {
			err = conn.WriteMessage(websocket.BinaryMessage, responseFrame)
		}
		if err != nil {
			results <- err
			return
		}
		_, ackFrame, err := conn.ReadMessage()
		if err == nil {
			var ack *packet.Packet
			ack, err = readPacket(bytes.NewReader(ackFrame))
			if err == nil && ack.Type != packet.HandshakeAck {
				err = fmt.Errorf("websocket handshake ack type = %v", ack.Type)
			}
		}
		results <- err
		if err == nil {
			_, _, _ = conn.ReadMessage()
		}
	}))
	t.Cleanup(server.Close)
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	return server, rootCAs, results
}

func TestConcurrentWSSConnectionsDoNotMutateDefaultDialerOrTLSConfig(t *testing.T) {
	const connectionCount = 8
	server, rootCAs, handshakes := newWSSHandshakeServer(t, connectionCount)
	uri := strings.Replace(server.URL, "https://", "wss://", 1)

	unknownCAClient := New()
	require.Error(t, unknownCAClient.ConnectTo(uri))

	defaultDialerTLSConfigBefore := websocket.DefaultDialer.TLSClientConfig
	clients := make([]*Client, connectionCount)
	configs := make([]tls.Config, connectionCount)
	errorsByClient := make(chan error, connectionCount)
	var workers sync.WaitGroup
	workers.Add(connectionCount)
	for i := range connectionCount {
		clients[i] = New()
		configs[i] = tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12}
		go func(index int) {
			defer workers.Done()
			errorsByClient <- clients[index].ConnectTo(uri, &configs[index])
		}(i)
	}
	workers.Wait()
	close(errorsByClient)
	for err := range errorsByClient {
		require.NoError(t, err)
	}
	for range connectionCount {
		require.NoError(t, <-handshakes)
	}
	for i := range connectionCount {
		assert.Empty(t, configs[i].ServerName)
		assert.False(t, configs[i].InsecureSkipVerify)
	}
	assert.Same(t, defaultDialerTLSConfigBefore, websocket.DefaultDialer.TLSClientConfig)

	for _, c := range clients {
		c.Disconnect(CloseReasonManual)
	}
}
