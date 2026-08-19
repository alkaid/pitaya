package modules

import (
	"crypto/tls"
	"path/filepath"
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestETCDBindingOwnedClientConfigIncludesTLSAndAuthentication(t *testing.T) {
	conf := config.NewDefaultPitayaConfig().Modules.BindingStorage.Etcd
	conf.User = "binding-user"
	conf.Pass = "binding-secret"
	conf.TLS = config.TLSConfig{Enabled: true, ServerName: "etcd.internal"}
	binding := NewETCDBindingStorage(nil, nil, conf)

	clientConfig, err := binding.ownedClientConfig()
	require.NoError(t, err)
	assert.Equal(t, conf.Endpoints, clientConfig.Endpoints)
	assert.Equal(t, conf.User, clientConfig.Username)
	assert.Equal(t, conf.Pass, clientConfig.Password)
	require.NotNil(t, clientConfig.TLS)
	assert.Equal(t, "etcd.internal", clientConfig.TLS.ServerName)
	assert.Equal(t, uint16(tls.VersionTLS12), clientConfig.TLS.MinVersion)
}

func TestETCDBindingInitRejectsInvalidTLSBeforeCreatingClient(t *testing.T) {
	conf := config.NewDefaultPitayaConfig().Modules.BindingStorage.Etcd
	secretPath := filepath.Join(t.TempDir(), "tenant-secret-ca.pem")
	conf.TLS = config.TLSConfig{Enabled: true, CAFile: secretPath}
	binding := NewETCDBindingStorage(nil, nil, conf)

	err := binding.Init()
	require.Error(t, err)
	assert.Nil(t, binding.cli)
	assert.Contains(t, err.Error(), "pitaya.modules.bindingstorage.etcd.tls.cafile")
	assert.NotContains(t, err.Error(), secretPath)
}
