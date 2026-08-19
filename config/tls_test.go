package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alkaid/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLSConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  TLSConfig
		want string
	}{
		{name: "disabled default"},
		{
			name: "disabled with CA",
			cfg:  TLSConfig{CAFile: "ca.pem"},
			want: "test.tls.enabled",
		},
		{
			name: "certificate without key",
			cfg:  TLSConfig{Enabled: true, CertFile: "client.pem"},
			want: "test.tls.certfile and test.tls.keyfile must be configured together",
		},
		{
			name: "blank configured value",
			cfg:  TLSConfig{Enabled: true, CAFile: " \t"},
			want: "test.tls.cafile must not be blank",
		},
		{
			name: "enabled with system roots",
			cfg:  TLSConfig{Enabled: true, ServerName: "redis.internal"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate("test.tls")
			if test.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestLoadClientTLSUsesSecureDefaultsAndStrictCustomCA(t *testing.T) {
	cfg, err := LoadClientTLS("test.tls", TLSConfig{})
	require.NoError(t, err)
	require.Nil(t, cfg)

	cfg, err = LoadClientTLS("test.tls", TLSConfig{
		Enabled:    true,
		ServerName: "redis.internal",
	})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Equal(t, "redis.internal", cfg.ServerName)
	assert.Nil(t, cfg.RootCAs, "nil RootCAs delegates to the platform system roots")

	certPEM, _ := newTestCertificate(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	cfg, err = LoadClientTLS("test.tls", TLSConfig{Enabled: true, CAFile: caFile})
	require.NoError(t, err)
	require.NotNil(t, cfg.RootCAs)
	assert.NotEmpty(t, cfg.RootCAs.Subjects())

	strictFile := filepath.Join(t.TempDir(), "strict-ca.pem")
	require.NoError(t, os.WriteFile(strictFile, append(certPEM, []byte("trailing garbage")...), 0o600))
	_, err = LoadClientTLS("test.tls", TLSConfig{Enabled: true, CAFile: strictFile})
	require.Error(t, err)
	assert.Equal(t, "test.tls.cafile does not contain valid certificate-only PEM", err.Error())
}

func TestLoadClientTLSLoadsClientCertificateWithoutLeakingFilePaths(t *testing.T) {
	certPEM, keyPEM := newTestCertificate(t)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.pem")
	keyFile := filepath.Join(dir, "client-key.pem")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	cfg, err := LoadClientTLS("test.tls", TLSConfig{
		Enabled:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
	})
	require.NoError(t, err)
	require.Len(t, cfg.Certificates, 1)

	secretPath := filepath.Join(t.TempDir(), "tenant-secret-ca.pem")
	_, err = LoadClientTLS("pitaya.storage.redis.tls", TLSConfig{
		Enabled: true,
		CAFile:  secretPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pitaya.storage.redis.tls.cafile")
	assert.NotContains(t, err.Error(), secretPath)
}

func TestNatsAuthConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  NatsAuthConfig
		want string
	}{
		{name: "anonymous"},
		{name: "username password", cfg: NatsAuthConfig{Username: "pitaya", Password: "secret"}},
		{name: "token", cfg: NatsAuthConfig{Token: "secret"}},
		{name: "credentials file", cfg: NatsAuthConfig{CredentialsFile: "client.creds"}},
		{
			name: "partial username password",
			cfg:  NatsAuthConfig{Username: "pitaya"},
			want: "test.auth.username and test.auth.password must be configured together",
		},
		{
			name: "mixed modes",
			cfg:  NatsAuthConfig{Token: "secret", CredentialsFile: "client.creds"},
			want: "test.auth must configure at most one authentication mode",
		},
		{
			name: "blank token",
			cfg:  NatsAuthConfig{Token: " \t"},
			want: "test.auth.token must not be blank",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate("test.auth")
			if test.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestNatsConfigValidationRejectsURLUserinfo(t *testing.T) {
	rpc := NewDefaultPitayaConfig().Cluster.RPC
	rpc.Client.Nats.Connect = "nats://user:password@localhost:4222"
	rpc.Server.Nats.Connect = "nats://localhost:4222,token@backup:4222"

	clientErr := rpc.Client.Nats.Validate()
	require.Error(t, clientErr)
	assert.Contains(t, clientErr.Error(), "pitaya.cluster.rpc.client.nats.connect must not contain URL userinfo")
	assert.NotContains(t, clientErr.Error(), "password")

	serverErr := rpc.Server.Nats.Validate()
	require.Error(t, serverErr)
	assert.Contains(t, serverErr.Error(), "pitaya.cluster.rpc.server.nats.connect must not contain URL userinfo")
	assert.NotContains(t, serverErr.Error(), "token")
}

func TestTLSAndAuthViperKeysUnmarshal(t *testing.T) {
	v := viper.New()
	v.Set("pitaya.cluster.rpc.client.nats.tls.enabled", true)
	v.Set("pitaya.cluster.rpc.client.nats.tls.servername", "nats.internal")
	v.Set("pitaya.cluster.rpc.client.nats.auth.credentialsfile", "client.creds")
	v.Set("pitaya.cluster.sd.etcd.tls.cafile", "etcd-ca.pem")
	v.Set("pitaya.groups.etcd.user", "groups-user")
	v.Set("pitaya.groups.etcd.pass", "groups-pass")
	v.Set("pitaya.modules.bindingstorage.etcd.tls.certfile", "binding.pem")
	v.Set("pitaya.storage.redis.tls.keyfile", "redis-key.pem")
	v.Set("pitaya.worker.redis.username", "worker-user")
	v.Set("pitaya.worker.redis.tls.enabled", true)

	cfg := NewPitayaConfig(NewConfig(v))
	assert.True(t, cfg.Cluster.RPC.Client.Nats.TLS.Enabled)
	assert.Equal(t, "nats.internal", cfg.Cluster.RPC.Client.Nats.TLS.ServerName)
	assert.Equal(t, "client.creds", cfg.Cluster.RPC.Client.Nats.Auth.CredentialsFile)
	assert.Equal(t, "etcd-ca.pem", cfg.Cluster.SD.Etcd.TLS.CAFile)
	assert.Equal(t, "groups-user", cfg.Groups.Etcd.User)
	assert.Equal(t, "groups-pass", cfg.Groups.Etcd.Pass)
	assert.Equal(t, "binding.pem", cfg.Modules.BindingStorage.Etcd.TLS.CertFile)
	assert.Equal(t, "redis-key.pem", cfg.Storage.Redis.TLS.KeyFile)
	assert.Equal(t, "worker-user", cfg.Worker.Redis.Username)
	assert.True(t, cfg.Worker.Redis.TLS.Enabled)
}

func TestTLSAndAuthViperDefaultsAreRegistered(t *testing.T) {
	cfg := NewConfig()
	prefixes := []string{
		"pitaya.cluster.rpc.client.nats.tls",
		"pitaya.cluster.rpc.server.nats.tls",
		"pitaya.cluster.sd.etcd.tls",
		"pitaya.groups.etcd.tls",
		"pitaya.modules.bindingstorage.etcd.tls",
		"pitaya.storage.redis.tls",
		"pitaya.worker.redis.tls",
	}
	for _, prefix := range prefixes {
		for _, field := range []string{"enabled", "cafile", "certfile", "keyfile", "servername"} {
			assert.True(t, cfg.IsSet(prefix+"."+field), prefix+"."+field)
		}
	}
	for _, prefix := range []string{
		"pitaya.cluster.rpc.client.nats.auth",
		"pitaya.cluster.rpc.server.nats.auth",
	} {
		for _, field := range []string{"username", "password", "token", "credentialsfile"} {
			assert.True(t, cfg.IsSet(prefix+"."+field), prefix+"."+field)
		}
	}
	for _, key := range []string{
		"pitaya.cluster.sd.etcd.user",
		"pitaya.cluster.sd.etcd.pass",
		"pitaya.groups.etcd.user",
		"pitaya.groups.etcd.pass",
		"pitaya.modules.bindingstorage.etcd.user",
		"pitaya.modules.bindingstorage.etcd.pass",
		"pitaya.worker.redis.username",
		"pitaya.worker.redis.password",
	} {
		assert.True(t, cfg.IsSet(key), key)
	}
}

func TestPitayaConfigValidatesEmbeddedTLSAndCredentials(t *testing.T) {
	tests := []struct {
		name    string
		cluster bool
		mutate  func(*PitayaConfig)
		want    string
	}{
		{
			name:   "storage redis disabled TLS fields",
			mutate: func(cfg *PitayaConfig) { cfg.Storage.Redis.TLS.CAFile = "ca.pem" },
			want:   "pitaya.storage.redis.tls.enabled",
		},
		{
			name:   "worker redis partial certificate",
			mutate: func(cfg *PitayaConfig) { cfg.Worker.Redis.TLS = TLSConfig{Enabled: true, CertFile: "client.pem"} },
			want:   "pitaya.worker.redis.tls.certfile",
		},
		{
			name:   "groups etcd partial credentials",
			mutate: func(cfg *PitayaConfig) { cfg.Groups.Etcd.User = "pitaya" },
			want:   "pitaya.groups.etcd.user and pass",
		},
		{
			name:   "binding etcd partial credentials",
			mutate: func(cfg *PitayaConfig) { cfg.Modules.BindingStorage.Etcd.Pass = "secret" },
			want:   "pitaya.modules.bindingstorage.etcd.user and pass",
		},
		{
			name:    "service discovery disabled TLS fields",
			cluster: true,
			mutate:  func(cfg *PitayaConfig) { cfg.Cluster.SD.Etcd.TLS.ServerName = "etcd.internal" },
			want:    "pitaya.cluster.sd.etcd.tls.enabled",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := NewDefaultPitayaConfig()
			test.mutate(cfg)
			var err error
			if test.cluster {
				err = cfg.ValidateCluster()
			} else {
				err = cfg.Validate()
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestRedisOptionConvertersPropagateTLS(t *testing.T) {
	redisConfig := NewDefaultPitayaConfig().Storage.Redis
	redisConfig.TLS = TLSConfig{Enabled: true, ServerName: "redis.internal"}

	node, err := NewRedisNodeOptions(&redisConfig)
	require.NoError(t, err)
	cluster, err := NewRedisClusterOptions(&redisConfig)
	require.NoError(t, err)
	universal, err := NewRedisUniversalOptions(&redisConfig)
	require.NoError(t, err)

	for name, clientTLS := range map[string]*tls.Config{
		"node":      node.TLSConfig,
		"cluster":   cluster.TLSConfig,
		"universal": universal.TLSConfig,
	} {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, clientTLS)
			assert.Equal(t, "redis.internal", clientTLS.ServerName)
			assert.Equal(t, uint16(tls.VersionTLS12), clientTLS.MinVersion)
		})
	}
}

func TestPitayaConfigValidatesRedisACLUsernames(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PitayaConfig)
		want   string
	}{
		{
			name: "session Redis username without password",
			mutate: func(cfg *PitayaConfig) {
				cfg.Storage.Redis.Username = "session-user"
			},
			want: "pitaya.storage.redis.password must be configured when username is set",
		},
		{
			name: "worker Redis blank username",
			mutate: func(cfg *PitayaConfig) {
				cfg.Worker.Redis.Username = " \t"
				cfg.Worker.Redis.Password = "secret"
			},
			want: "pitaya.worker.redis.username must not be blank",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := NewDefaultPitayaConfig()
			test.mutate(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
			assert.NotContains(t, err.Error(), "secret")
		})
	}

	cfg := NewDefaultPitayaConfig()
	cfg.Storage.Redis.Password = "default-user-secret"
	require.NoError(t, cfg.Validate(), "password-only Redis auth targets the default user")
}

func TestLegacyRedisOptionConvertersFailClosed(t *testing.T) {
	redisConfig := NewDefaultPitayaConfig().Storage.Redis
	secretPath := filepath.Join(t.TempDir(), "tenant-secret-ca.pem")
	redisConfig.TLS = TLSConfig{Enabled: true, CAFile: secretPath}

	for name, convert := range map[string]func(){
		"node":      func() { _ = ToRedisNodeConfig(&redisConfig) },
		"cluster":   func() { _ = ToRedisClusterOption(&redisConfig) },
		"universal": func() { _ = ToRedisUniversalOptions(&redisConfig) },
	} {
		t.Run(name, func(t *testing.T) {
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				convert()
			}()
			require.NotNil(t, recovered)
			assert.NotContains(t, fmt.Sprint(recovered), secretPath)
		})
	}
}

func newTestCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pitaya-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
