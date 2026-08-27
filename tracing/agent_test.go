package tracing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
)

type fakeTraceCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	exports chan metadata.MD
}

func (collector *fakeTraceCollector) Export(
	ctx context.Context,
	_ *collectortrace.ExportTraceServiceRequest,
) (*collectortrace.ExportTraceServiceResponse, error) {
	headers, _ := metadata.FromIncomingContext(ctx)
	collector.exports <- headers.Copy()
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type traceConnectionTracker struct {
	active atomic.Int64
}

func (*traceConnectionTracker) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (*traceConnectionTracker) HandleRPC(context.Context, stats.RPCStats) {}

func (*traceConnectionTracker) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (tracker *traceConnectionTracker) HandleConn(_ context.Context, event stats.ConnStats) {
	switch event.(type) {
	case *stats.ConnBegin:
		tracker.active.Add(1)
	case *stats.ConnEnd:
		tracker.active.Add(-1)
	}
}

type traceTestPKI struct {
	caFile         string
	clientCertFile string
	clientKeyFile  string
	serverCert     tls.Certificate
	caPool         *x509.CertPool
}

func newTraceTestPKI(t *testing.T) traceTestPKI {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Pitaya test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCertificate, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	createLeaf := func(serial int64, commonName string, usages []x509.ExtKeyUsage, dnsNames []string) ([]byte, []byte) {
		t.Helper()
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, keyErr)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: commonName},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  usages,
			DNSNames:     dnsNames,
		}
		certificateDER, createErr := x509.CreateCertificate(
			rand.Reader,
			template,
			caCertificate,
			&key.PublicKey,
			caKey,
		)
		require.NoError(t, createErr)
		keyDER, marshalErr := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, marshalErr)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}

	serverCertPEM, serverKeyPEM := createLeaf(
		2,
		"collector.test",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		[]string{"collector.test"},
	)
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	require.NoError(t, err)
	clientCertPEM, clientKeyPEM := createLeaf(
		3,
		"pitaya-client",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		nil,
	)

	directory := t.TempDir()
	write := func(name string, contents []byte) string {
		t.Helper()
		path := filepath.Join(directory, name)
		require.NoError(t, os.WriteFile(path, contents, 0o600))
		return path
	}
	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(caPEM))
	return traceTestPKI{
		caFile:         write("ca.pem", caPEM),
		clientCertFile: write("client.pem", clientCertPEM),
		clientKeyFile:  write("client-key.pem", clientKeyPEM),
		serverCert:     serverCert,
		caPool:         caPool,
	}
}

func startFakeTraceCollector(
	t *testing.T,
	tlsConfig *tls.Config,
) (string, *fakeTraceCollector, *traceConnectionTracker) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tracker := &traceConnectionTracker{}
	options := []grpc.ServerOption{grpc.StatsHandler(tracker)}
	if tlsConfig != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}
	server := grpc.NewServer(options...)
	collector := &fakeTraceCollector{exports: make(chan metadata.MD, 8)}
	collectortrace.RegisterTraceServiceServer(server, collector)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String(), collector, tracker
}

func startAgentForTest(t *testing.T, cfg Config) {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	require.NoError(t, StartAgent(context.Background(), cfg))
	t.Cleanup(func() {
		_ = StopAgent()
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})
}

func exportTestSpan(timeout time.Duration) error {
	_, span := otel.Tracer(TraceName).Start(context.Background(), "agent-test")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return agentInstance.ForceFlush(ctx)
}

func awaitTraceExport(t *testing.T, collector *fakeTraceCollector) metadata.MD {
	t.Helper()
	select {
	case headers := <-collector.exports:
		return headers
	case <-time.After(3 * time.Second):
		t.Fatal("collector did not receive a trace export")
		return nil
	}
}

func TestAgentPlaintextHeadersRestartAndStop(t *testing.T) {
	endpoint, collector, tracker := startFakeTraceCollector(t, nil)
	headers := map[string]string{"authorization": "Bearer original"}
	startAgentForTest(t, Config{
		Name:     "plaintext",
		Endpoint: endpoint,
		Sampler:  1,
		Headers:  headers,
	})
	headers["authorization"] = "Bearer mutated"
	require.NoError(t, exportTestSpan(3*time.Second))
	assert.Equal(t, []string{"Bearer original"}, awaitTraceExport(t, collector).Get("authorization"))

	require.NoError(t, StartAgent(context.Background(), Config{
		Name:     "plaintext-restart",
		Endpoint: endpoint,
		Sampler:  1,
	}))
	require.NoError(t, exportTestSpan(3*time.Second))
	awaitTraceExport(t, collector)
	require.NoError(t, StopAgent())
	require.Eventually(t, func() bool {
		return tracker.active.Load() == 0
	}, time.Second, time.Millisecond)
	require.Nil(t, agentInstance)
}

func TestAgentTLSAndMTLS(t *testing.T) {
	tests := []struct {
		name      string
		serverTLS func(traceTestPKI) *tls.Config
		clientTLS func(traceTestPKI) config.TLSConfig
	}{
		{
			name: "trusted ca",
			serverTLS: func(pki traceTestPKI) *tls.Config {
				return &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{pki.serverCert},
				}
			},
			clientTLS: func(pki traceTestPKI) config.TLSConfig {
				return config.TLSConfig{
					Enabled:    true,
					CAFile:     pki.caFile,
					ServerName: "collector.test",
				}
			},
		},
		{
			name: "mutual tls",
			serverTLS: func(pki traceTestPKI) *tls.Config {
				return &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{pki.serverCert},
					ClientAuth:   tls.RequireAndVerifyClientCert,
					ClientCAs:    pki.caPool,
				}
			},
			clientTLS: func(pki traceTestPKI) config.TLSConfig {
				return config.TLSConfig{
					Enabled:    true,
					CAFile:     pki.caFile,
					CertFile:   pki.clientCertFile,
					KeyFile:    pki.clientKeyFile,
					ServerName: "collector.test",
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, StopAgent())
			pki := newTraceTestPKI(t)
			endpoint, collector, tracker := startFakeTraceCollector(t, test.serverTLS(pki))
			startAgentForTest(t, Config{
				Name:     test.name,
				Endpoint: endpoint,
				Sampler:  1,
				TLS:      test.clientTLS(pki),
			})
			require.NoError(t, exportTestSpan(3*time.Second))
			awaitTraceExport(t, collector)
			require.NoError(t, StopAgent())
			require.Eventually(t, func() bool {
				return tracker.active.Load() == 0
			}, time.Second, time.Millisecond)
		})
	}
}

func TestAgentTLSFailuresSurfaceOnExport(t *testing.T) {
	tests := []struct {
		name      string
		clientTLS func(traceTestPKI) config.TLSConfig
	}{
		{
			name: "unknown ca",
			clientTLS: func(traceTestPKI) config.TLSConfig {
				return config.TLSConfig{Enabled: true, ServerName: "collector.test"}
			},
		},
		{
			name: "hostname mismatch",
			clientTLS: func(pki traceTestPKI) config.TLSConfig {
				return config.TLSConfig{
					Enabled:    true,
					CAFile:     pki.caFile,
					ServerName: "wrong.test",
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, StopAgent())
			pki := newTraceTestPKI(t)
			endpoint, _, tracker := startFakeTraceCollector(t, &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{pki.serverCert},
			})
			startAgentForTest(t, Config{
				Name:     test.name,
				Endpoint: endpoint,
				Sampler:  1,
				TLS:      test.clientTLS(pki),
			})
			require.Error(t, exportTestSpan(750*time.Millisecond))
			_ = StopAgent()
			require.Nil(t, agentInstance)
			require.Eventually(t, func() bool {
				return tracker.active.Load() == 0
			}, time.Second, time.Millisecond)
		})
	}
}

func TestAgentRejectsInvalidTLSConfigurationWithoutState(t *testing.T) {
	require.NoError(t, StopAgent())
	err := StartAgent(context.Background(), Config{
		Name: "invalid-tls",
		TLS: config.TLSConfig{
			Enabled:  true,
			CertFile: "client.pem",
		},
	})
	require.Error(t, err)
	require.Nil(t, agentInstance)
}
