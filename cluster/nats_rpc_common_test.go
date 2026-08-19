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

package cluster

import (
	"crypto/tls"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/helpers"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsservertest "github.com/nats-io/nats-server/v2/test"
	nats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getServer() *Server {
	return &Server{
		ID:       "id1",
		Type:     "type1",
		Frontend: true,
	}
}

func runTestNATSServerAtPort(t testing.TB, port int) *natsserver.Server {
	t.Helper()
	opts := natsservertest.DefaultTestOptions
	opts.Port = port
	return natsservertest.RunServer(&opts)
}

func TestNatsRPCCommonGetChannel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "pitaya/servers/type1/sv1", getChannel("type1", "sv1"))
	assert.Equal(t, "pitaya/servers/2type1/2sv1", getChannel("2type1", "2sv1"))
}

func TestSignalAppDieIsIdempotentAndNonBlocking(t *testing.T) {
	full := make(chan bool, 1)
	full <- true
	done := make(chan struct{})
	go func() {
		defer close(done)
		signalAppDie(full)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signalAppDie blocked on a full channel")
	}

	closed := make(chan bool)
	close(closed)
	assert.NotPanics(t, func() { signalAppDie(closed) })
}

func TestNatsRPCCommonSetupNatsConn(t *testing.T) {
	t.Parallel()
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), nil)
	assert.NoError(t, err)
	assert.NotNil(t, conn)
}

func TestBuildNATSConnectionOptionsEnablesStrictTLS(t *testing.T) {
	options, err := buildNATSConnectionOptions(
		"pitaya.cluster.rpc.client.nats",
		config.TLSConfig{Enabled: true, ServerName: "nats.internal"},
		config.NatsAuthConfig{},
	)
	require.NoError(t, err)

	applied := nats.GetDefaultOptions()
	for _, option := range options {
		require.NoError(t, option(&applied))
	}
	require.NotNil(t, applied.TLSConfig)
	assert.True(t, applied.Secure)
	assert.Equal(t, "nats.internal", applied.TLSConfig.ServerName)
	assert.Equal(t, uint16(tls.VersionTLS12), applied.TLSConfig.MinVersion)
}

func TestBuildNATSConnectionOptionsRejectsUnreadableCredentialsBeforeDial(t *testing.T) {
	credentialsFile := filepath.Join(t.TempDir(), "tenant-secret.creds")
	_, err := buildNATSConnectionOptions(
		"pitaya.cluster.rpc.server.nats",
		config.TLSConfig{},
		config.NatsAuthConfig{CredentialsFile: credentialsFile},
	)
	require.Error(t, err)
	assert.Equal(t, "pitaya.cluster.rpc.server.nats.auth.credentialsfile could not be read", err.Error())
	assert.NotContains(t, err.Error(), credentialsFile)
}

func TestNATSConnectionOptionsAuthenticateWithoutURLUserinfo(t *testing.T) {
	opts := natsservertest.DefaultTestOptions
	opts.Port = -1
	opts.Username = "pitaya"
	opts.Password = "transport-secret"
	server := natsservertest.RunServer(&opts)
	defer server.Shutdown()

	options, err := buildNATSConnectionOptions(
		"pitaya.cluster.rpc.client.nats",
		config.TLSConfig{},
		config.NatsAuthConfig{Username: opts.Username, Password: opts.Password},
	)
	require.NoError(t, err)
	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", server.Addr()), nil, options...)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.True(t, conn.IsConnected())
}

func TestNatsRPCCommonSetupNatsConnShouldError(t *testing.T) {
	t.Parallel()
	conn, err := setupNatsConn("nats://localhost:1234", nil)
	assert.Error(t, err)
	assert.Nil(t, conn)
}

func TestNatsRPCCommonCloseHandler(t *testing.T) {
	t.Parallel()
	s := helpers.GetTestNatsServer(t)

	dieChan := make(chan bool, 1)

	conn, err := setupNatsConn(fmt.Sprintf("nats://%s", s.Addr()), dieChan, nats.MaxReconnects(1),
		nats.ReconnectWait(1*time.Millisecond))
	assert.NoError(t, err)
	assert.NotNil(t, conn)

	s.Shutdown()

	value, ok := <-dieChan
	assert.True(t, ok)
	assert.True(t, value)
}

func TestNatsRPCCommonUnexpectedNormalCloseSignalsAppDie(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	dieChan := make(chan bool, 1)
	conn, terminalDone, err := setupTrackedNatsConn(
		fmt.Sprintf("nats://%s", s.Addr()),
		dieChan,
		nil,
	)
	assert.NoError(t, err)

	conn.Close()
	select {
	case <-terminalDone:
	case <-time.After(time.Second):
		t.Fatal("terminal close callback did not finish")
	}
	select {
	case <-dieChan:
	case <-time.After(time.Second):
		t.Fatal("unexpected normal connection close did not signal app failure")
	}
}

func TestTrackedNatsConnPlannedCloseDoesNotSignalAppDie(t *testing.T) {
	s := helpers.GetTestNatsServer(t)
	defer s.Shutdown()
	dieChan := make(chan bool, 1)
	conn, terminalDone, err := setupTrackedNatsConnWithLifecycle(
		fmt.Sprintf("nats://%s", s.Addr()),
		dieChan,
		natsConnLifecycleCallbacks{closing: func() bool { return true }},
	)
	assert.NoError(t, err)

	conn.Close()
	select {
	case <-terminalDone:
	case <-time.After(time.Second):
		t.Fatal("terminal close callback did not finish")
	}
	select {
	case <-dieChan:
		t.Fatal("planned connection close signaled app failure")
	default:
	}
}
