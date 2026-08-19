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

package groups

import (
	"context"
	"crypto/tls"
	"path/filepath"
	"sync"
	"testing"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/assert"
	"go.etcd.io/etcd/tests/v3/framework/integration"
)

func TestEtcdGroupOwnedClientConfigIncludesTLSAndAuthentication(t *testing.T) {
	conf := config.NewDefaultPitayaConfig().Groups.Etcd
	conf.User = "groups-user"
	conf.Pass = "groups-secret"
	conf.TLS = config.TLSConfig{Enabled: true, ServerName: "etcd.internal"}

	clientConfig, err := buildEtcdGroupClientConfig(conf)
	assert.NoError(t, err)
	assert.Equal(t, conf.Endpoints, clientConfig.Endpoints)
	assert.Equal(t, conf.User, clientConfig.Username)
	assert.Equal(t, conf.Pass, clientConfig.Password)
	if assert.NotNil(t, clientConfig.TLS) {
		assert.Equal(t, "etcd.internal", clientConfig.TLS.ServerName)
		assert.Equal(t, uint16(tls.VersionTLS12), clientConfig.TLS.MinVersion)
	}
}

func TestEtcdGroupInjectedClientOwnsTransportSecurity(t *testing.T) {
	etcdCluster, cli := helpers.GetTestEtcd(t)
	defer etcdCluster.Terminate(t)
	clientInstance = nil
	transactionTimeout = 0
	etcdOnce = sync.Once{}
	etcdInitErr = nil

	conf := config.NewDefaultPitayaConfig().Groups.Etcd
	conf.TLS = config.TLSConfig{
		Enabled: true,
		CAFile:  filepath.Join(t.TempDir(), "not-owned-by-groups.pem"),
	}
	service, err := NewEtcdGroupService(conf, cli)
	assert.NoError(t, err)
	assert.NotNil(t, service)
	assert.Same(t, cli, clientInstance)
}

func TestEtcdGroupInitializationReturnsStableTLSFailure(t *testing.T) {
	clientInstance = nil
	transactionTimeout = 0
	etcdOnce = sync.Once{}
	etcdInitErr = nil
	conf := config.NewDefaultPitayaConfig().Groups.Etcd
	secretPath := filepath.Join(t.TempDir(), "tenant-secret-ca.pem")
	conf.TLS = config.TLSConfig{Enabled: true, CAFile: secretPath}

	first, firstErr := NewEtcdGroupService(conf, nil)
	second, secondErr := NewEtcdGroupService(conf, nil)
	assert.Nil(t, first)
	assert.Nil(t, second)
	assert.Error(t, firstErr)
	assert.EqualError(t, secondErr, firstErr.Error())
	assert.Contains(t, firstErr.Error(), "pitaya.groups.etcd.tls.cafile")
	assert.NotContains(t, firstErr.Error(), secretPath)
}

func setup(t *testing.T) (*integration.Cluster, GroupService) {
	cluster, cli := helpers.GetTestEtcd(t)

	clientInstance = nil
	transactionTimeout = 0
	etcdOnce = sync.Once{}
	etcdInitErr = nil

	etcdGroupService, err := NewEtcdGroupService(config.NewDefaultPitayaConfig().Groups.Etcd, cli)
	if err != nil {
		panic(err)
	}

	return cluster, etcdGroupService
}

func TestEtcdCreateDuplicatedGroup(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testCreateDuplicatedGroup(etcdGroupService, t)
}

func TestEtcdCreateGroup(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testCreateGroup(etcdGroupService, t)
}

func TestEtcdCreateGroupWithTTL(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testCreateGroupWithTTL(etcdGroupService, t)
}

func TestEtcdGroupAddMember(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testGroupAddMember(etcdGroupService, t)
}

func TestEtcdGroupAddDuplicatedMember(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testGroupAddDuplicatedMember(etcdGroupService, t)
}

func TestEtcdGroupContainsMember(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testGroupContainsMember(etcdGroupService, t)
}

func TestEtcdRemove(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testRemove(etcdGroupService, t)
}

func TestEtcdDelete(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testDelete(etcdGroupService, t)
}

func TestEtcdRemoveAll(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testRemoveAll(etcdGroupService, t)
}

func TestEtcdCount(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testCount(etcdGroupService, t)
}

func TestEtcdMembers(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testMembers(etcdGroupService, t)
}

func TestEtcdMembersPage(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testMembersPage(etcdGroupService, t)
}

func TestEtcdEmptyUIDRejected(t *testing.T) {
	cluster, etcdGroupService := setup(t)
	defer cluster.Terminate(t)
	testEmptyUIDRejected(etcdGroupService, t)
}

func TestEtcdMembersPageRejectsStoredEmptyUID(t *testing.T) {
	cluster, groupService := setup(t)
	defer cluster.Terminate(t)
	ctx := context.Background()
	groupName := "testEtcdMembersPageRejectsStoredEmptyUID"
	assert.NoError(t, groupService.GroupCreate(ctx, groupName))
	_, err := clientInstance.Put(ctx, memberKey(groupName, ""), "")
	assert.NoError(t, err)
	_, err = clientInstance.Put(ctx, memberKey(groupName, "b"), "")
	assert.NoError(t, err)

	page, err := groupService.(PagedGroupService).GroupMembersPage(
		ctx,
		groupName,
		MemberPageOptions{Limit: 1},
	)
	assert.ErrorIs(t, err, constants.ErrEmptyUID)
	assert.Empty(t, page)
}
