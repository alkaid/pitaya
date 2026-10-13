package client

import (
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientTLSHostnameSNI(t *testing.T) {
	certificate, _ := newTestServerCertificate(t)
	for _, scheme := range []string{"tls", "tcps"} {
		for _, test := range []struct {
			name       string
			config     *tls.Config
			serverName string
		}{
			{name: "default config", serverName: "localhost"},
			{name: "infer hostname", config: &tls.Config{}, serverName: "localhost"},
			{name: "explicit override", config: &tls.Config{ServerName: "override.example"}, serverName: "override.example"},
		} {
			t.Run(scheme+"/"+test.name, func(t *testing.T) {
				observedName := make(chan string, 1)
				listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
					Certificates: []tls.Certificate{certificate},
					GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
						observedName <- hello.ServerName
						if hello.ServerName != test.serverName {
							return nil, fmt.Errorf("unexpected SNI %q, want %q", hello.ServerName, test.serverName)
						}
						return nil, nil
					},
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
				serverDone := make(chan error, 1)
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						serverDone <- err
						return
					}
					defer conn.Close()
					if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
						serverDone <- err
						return
					}
					if err := serveClientHandshake(conn); err != nil {
						serverDone <- err
						return
					}
					serverDone <- nil
					_, _ = readPacket(conn)
				}()

				c := New()
				t.Cleanup(func() { c.Disconnect(CloseReasonManual) })
				_, port, err := net.SplitHostPort(listener.Addr().String())
				require.NoError(t, err)
				uri := scheme + "://" + net.JoinHostPort("localhost", port)
				var configs []*tls.Config
				var originalServerName string
				if test.config != nil {
					originalServerName = test.config.ServerName
					configs = append(configs, test.config)
				}
				assert.NoError(t, c.ConnectTo(uri, configs...))
				select {
				case name := <-observedName:
					assert.Equal(t, test.serverName, name)
				case <-time.After(time.Second):
					t.Fatal("server did not receive a TLS ClientHello")
				}
				select {
				case err := <-serverDone:
					assert.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("server handshake did not finish")
				}
				if test.config != nil {
					assert.Equal(t, originalServerName, test.config.ServerName)
					assert.False(t, test.config.InsecureSkipVerify, "the caller's TLS configuration must remain unchanged")
				}
			})
		}
	}
}
