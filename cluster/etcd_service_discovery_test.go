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
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/assert"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var etcdSDTables = []struct {
	server *Server
}{
	{NewServer("frontend-1", "type1", true, map[string]string{"k1": "v1"})},
	{NewServer("backend-1", "type2", false, map[string]string{"k2": "v2"})},
	{NewServer("backend-2", "type3", false, map[string]string{})},
}

var etcdSDTablesMultipleServers = []struct {
	name    string
	servers []*Server
}{
	{"empty", []*Server{}},
	{"multiple-types", []*Server{
		NewServer("frontend-1", "type1", true, map[string]string{"k1": "v1"}),
		NewServer("backend-1", "type2", false, map[string]string{"k2": "v2"}),
		NewServer("backend-2", "type3", false, map[string]string{}),
	}},
	{"same-type", []*Server{
		NewServer("frontend-1", "type2", true, map[string]string{"k1": "v1"}),
		NewServer("frontend-2", "type2", true, map[string]string{"k2": "v2"}),
		NewServer("frontend-3", "type2", true, map[string]string{"k1": "v1"}),
	}},
}

var etcdSDBlacklistTables = []struct {
	name                string
	server              *Server
	serversToAdd        []*Server
	serverTypeBlacklist []string
}{
	{
		name:   "test1",
		server: NewServer("frontend-1", "type1", true, nil),
		serversToAdd: []*Server{
			NewServer("frontend-1", "type1", true, nil),
		},
		serverTypeBlacklist: nil,
	},
	{
		name:   "test2",
		server: NewServer("frontend-1", "type1", true, nil),
		serversToAdd: []*Server{
			NewServer("backend-1", "type1", false, nil),
			NewServer("backend-2", "type2", false, nil),
			NewServer("backend-3", "type3", false, nil),
		},
		serverTypeBlacklist: []string{"type2"},
	},
	{
		name:   "test3",
		server: NewServer("frontend-1", "type1", true, nil),
		serversToAdd: []*Server{
			NewServer("backend-1", "type1", false, nil),
			NewServer("backend-2", "type2", false, nil),
			NewServer("backend-3", "type3", false, nil),
			NewServer("backend-4", "type4", false, nil),
		},
		serverTypeBlacklist: []string{"type1", "type4"},
	},
}

func getEtcdSD(t *testing.T, config config.EtcdServiceDiscoveryConfig, server *Server, cli *clientv3.Client) *etcdServiceDiscovery {
	t.Helper()
	appDieChan := make(chan bool)
	e, err := NewEtcdServiceDiscovery(config, server, appDieChan, cli)
	assert.NoError(t, err)
	return e.(*etcdServiceDiscovery)
}

func TestNewEtcdServiceDiscovery(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			assert.NotNil(t, e)
		})
	}
}

func TestEtcdSDBootstrapLease(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			err := e.grantLease()
			assert.NoError(t, err)
			assert.NotEmpty(t, e.leaseID)
		})
	}
}

func TestEtcdSDBootstrapLeaseError(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.etcdDialTimeout = -time.Nanosecond
			err := e.grantLease()
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

func TestEtcdSDBootstrapServer(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			err := e.bootstrapServer(table.server)
			assert.NoError(t, err)
			v, err := cli.Get(context.TODO(), getKey(table.server.ID, table.server.Type))
			assert.NoError(t, err)
			assert.NotNil(t, v)
			assert.Equal(t, 1, len(v.Kvs))
			generatedSv, ok := e.serverMapByID.Load(table.server.ID)
			assert.True(t, ok)
			assert.Equal(t, table.server, generatedSv)
			val := v.Kvs[0]
			assert.Equal(t, getKey(table.server.ID, table.server.Type), string(val.Key))
			assert.Equal(t, table.server.AsJSONString(), string(val.Value))
		})
	}
}

func TestEtcdSDDeleteServer(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			err := e.bootstrapServer(table.server)
			assert.NoError(t, err)
			e.deleteServer(table.server.ID)
			e.serverMapByID.Delete(table.server.ID)
			generatedSv, ok := e.serverMapByID.Load(table.server.ID)
			assert.False(t, ok)
			assert.Nil(t, generatedSv)
			_, err = e.GetServersByType(table.server.Type)
			assert.ErrorIs(t, err, constants.ErrNoServersAvailableOfType)
		})
	}
}

func TestEtcdSDConsistentHashLifecycle(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	_, err := sd.GetConsistentHashNode("room", "session-1")
	assert.ErrorIs(t, err, constants.ErrServerNotFound)

	server := NewServer("room-1", "room", false, nil)
	sd.addServer(server)
	node, err := sd.GetConsistentHashNode(server.Type, "session-1")
	assert.NoError(t, err)
	assert.Equal(t, server.ID, node)

	sd.deleteServer(server.ID)
	_, err = sd.GetConsistentHashNode(server.Type, "session-1")
	assert.ErrorIs(t, err, constants.ErrServerNotFound)
}

func TestEtcdSDConsistentHashConcurrentFirstServers(t *testing.T) {
	for removedServer := 0; removedServer < 2; removedServer++ {
		t.Run(fmt.Sprintf("remove-%d", removedServer), func(t *testing.T) {
			cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			self := NewServer("frontend-self", "frontend", true, nil)
			sd := getEtcdSD(t, cfg, self, nil)
			servers := []*Server{
				NewServer("room-1", "room", false, nil),
				NewServer("room-2", "room", false, nil),
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, server := range servers {
				server := server
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					sd.addServer(server)
				}()
			}
			close(start)
			wg.Wait()

			sd.deleteServer(servers[removedServer].ID)
			remainingServer := servers[1-removedServer]
			node, err := sd.GetConsistentHashNode(remainingServer.Type, "session-1")
			assert.NoError(t, err)
			assert.Equal(t, remainingServer.ID, node)

			sd.deleteServer(remainingServer.ID)
			_, err = sd.GetConsistentHashNode(remainingServer.Type, "session-1")
			assert.ErrorIs(t, err, constants.ErrServerNotFound)
		})
	}
}

func TestEtcdSDConsistentHashConcurrentMutationAndLookup(t *testing.T) {
	cfg := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
	self := NewServer("frontend-self", "frontend", true, nil)
	sd := getEtcdSD(t, cfg, self, nil)

	const (
		typesPerRound = 32
		rounds        = 8
		lookupWorkers = 8
	)

	serverTypes := make([]string, 0, rounds*typesPerRound)
	for round := 0; round < rounds; round++ {
		for typeIndex := 0; typeIndex < typesPerRound; typeIndex++ {
			serverTypes = append(serverTypes, fmt.Sprintf("room-%d-%d", round, typeIndex))
		}
	}

	start := make(chan struct{})
	unexpectedErrors := make(chan error, lookupWorkers*len(serverTypes)*rounds)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for round := 0; round < rounds; round++ {
			for typeIndex := 0; typeIndex < typesPerRound; typeIndex++ {
				serverType := fmt.Sprintf("room-%d-%d", round, typeIndex)
				sd.addServer(NewServer(serverType+"-1", serverType, false, nil))
				runtime.Gosched()
			}
			// This is the cleanup path used by SyncServers after applying its snapshot.
			sd.deleteLocalInvalidServers(nil)
		}
	}()

	for worker := 0; worker < lookupWorkers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for round := 0; round < rounds; round++ {
				for _, serverType := range serverTypes {
					node, err := sd.GetConsistentHashNode(serverType, fmt.Sprintf("session-%d-%d", worker, round))
					if err != nil && !errors.Is(err, constants.ErrServerNotFound) {
						unexpectedErrors <- err
					}
					if err == nil && node != serverType+"-1" {
						unexpectedErrors <- fmt.Errorf("unexpected node %q returned for %s", node, serverType)
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(unexpectedErrors)
	for err := range unexpectedErrors {
		assert.NoError(t, err)
	}

	sd.deleteLocalInvalidServers(nil)
	for _, serverType := range serverTypes {
		_, err := sd.GetConsistentHashNode(serverType, "session-final")
		assert.ErrorIs(t, err, constants.ErrServerNotFound)
	}
}

func TestEtcdSDGetKey(t *testing.T) {
	t.Parallel()
	tables := []struct {
		serverType string
		serverID   string
		ret        string
	}{
		{"type1", "id1", "servers/type1/id1"},
		{"t", "1", "servers/t/1"},
	}

	for _, table := range tables {
		t.Run(table.ret, func(t *testing.T) {
			assert.Equal(t, table.ret, getKey(table.serverID, table.serverType))
		})
	}
}

func TestEtcdSDDeleteLocalInvalidServers(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			invalidServer := &Server{
				ID:   "invalid",
				Type: "bla",
			}
			e.addServer(invalidServer)
			e.deleteLocalInvalidServers([]string{table.server.ID})
			inv, err := e.GetServer(invalidServer.ID)
			assert.ErrorIs(t, err, constants.ErrNoServerWithID)
			assert.Nil(t, inv)
		})
	}
}

func TestEtcdSDGetServer(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			e.bootstrapServer(table.server)
			sv, err := e.GetServer(table.server.ID)
			assert.NoError(t, err)
			assert.Equal(t, table.server, sv)
		})
	}
}

func TestEtcdSDGetServers(t *testing.T) {
	for _, table := range etcdSDTablesMultipleServers {
		t.Run(table.name, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			self := &Server{}
			self.fix()
			e := getEtcdSD(t, config, self, cli)
			e.Init()
			for _, server := range table.servers {
				e.bootstrapServer(server)
			}
			serverList := e.GetServers()
			var checkList []*Server
			checkList = append(table.servers, self)
			assert.ElementsMatch(t, checkList, serverList)
		})
	}
}

func TestEtcdSDInit(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = time.Duration(300 * time.Millisecond)
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			// should set running
			assert.True(t, e.running)
			// should have a leaseid
			assert.NotEmpty(t, e.leaseID)
			// should register the server
			sv, err := e.GetServer(table.server.ID)
			assert.NoError(t, err)
			assert.Equal(t, table.server, sv)
			// should heartbeat and sync
			time.Sleep(50 * time.Millisecond)
			// TODO may be flaky
			helpers.ShouldEventuallyReturn(t, func() bool {
				return math.Abs(float64(time.Now().Unix()-e.lastSyncTime.Unix())) < 5
			}, true, 50*time.Millisecond, 2*time.Second)
		})
	}
}

func TestEtcdBeforeShutdown(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			assert.True(t, e.running)
			e.BeforeShutdown()
			assert.True(t, e.running)
			_, err := cli.Revoke(context.TODO(), e.leaseID)
			assert.Error(t, err)
		})
	}
}

func TestEtcdShutdown(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			assert.True(t, e.running)
			e.Shutdown()
			assert.False(t, e.running)
		})
	}
}

func TestEtcdWatchChangesAddNewServers(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = time.Duration(100 * time.Millisecond)
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			e.running = true
			serversBefore, err := e.GetServersByType(table.server.Type)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(serversBefore))
			newServer := NewServer("newID", table.server.Type, false, map[string]string{})
			err = e.addServerIntoEtcd(newServer)
			assert.NoError(t, err)
			ss, err := getServerFromEtcd(e.cli, newServer.Type, newServer.ID)
			assert.NoError(t, err)
			assert.Equal(t, newServer, ss)
			helpers.ShouldEventuallyReturn(t, func() int {
				serversNow, _ := e.GetServersByType(table.server.Type)
				return len(serversNow)
			}, 2)
		})
	}
}

func TestEtcdWatchChangesDeleteServers(t *testing.T) {
	for _, table := range etcdSDTables {
		t.Run(table.server.ID, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = 100 * time.Millisecond
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			e.running = true
			serversBefore, err := e.GetServersByType(table.server.Type)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(serversBefore))
			newServer := NewServer("newID", table.server.Type, false, map[string]string{})
			err = e.addServerIntoEtcd(newServer)
			assert.NoError(t, err)
			ss, err := getServerFromEtcd(e.cli, newServer.Type, newServer.ID)
			assert.NoError(t, err)
			assert.Equal(t, newServer, ss)
			helpers.ShouldEventuallyReturn(t, func() int {
				serversNow, _ := e.GetServersByType(table.server.Type)
				return len(serversNow)
			}, 2)
			_, err = cli.Delete(context.TODO(), getKey(newServer.ID, newServer.Type))
			assert.NoError(t, err)
			helpers.ShouldEventuallyReturn(t, func() int {
				serversNow, _ := e.GetServersByType(table.server.Type)
				return len(serversNow)
			}, 1)
		})
	}
}

func TestEtcdWatchChangesWithBlacklist(t *testing.T) {
	for _, table := range etcdSDBlacklistTables {
		t.Run(table.name, func(t *testing.T) {
			config := config.NewDefaultPitayaConfig().Cluster.SD.Etcd
			config.SyncServers.Interval = 100 * time.Millisecond
			config.ServerTypesBlacklist = table.serverTypeBlacklist
			c, cli := helpers.GetTestEtcd(t)
			defer c.Terminate(t)
			e := getEtcdSD(t, config, table.server, cli)
			e.Init()
			e.running = true

			serversBefore, err := e.GetServersByType(table.server.Type)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(serversBefore))

			// Add all servers to ETCD
			for _, serverToAdd := range table.serversToAdd {
				_, err := cli.Put(
					context.TODO(),
					getKey(serverToAdd.ID, serverToAdd.Type),
					serverToAdd.AsJSONString(),
				)
				assert.NoError(t, err)
			}

			numRequiredServersInSd := func() int {
				num := 0
				for _, serverToAdd := range table.serversToAdd {
					if !e.isServerTypeBlacklisted(serverToAdd.Type) {
						num++
					}
				}
				return num + 1 // The server itself is always added to the cache, that's why we add one here
			}

			numServersInSd := func() int {
				num := 0

				_, err := e.GetServer(table.server.ID)
				if err == nil {
					num++
				}
				for _, serverToAdd := range table.serversToAdd {
					_, err := e.GetServer(serverToAdd.ID)
					if err == nil {
						num++
					}
				}
				return num
			}

			helpers.ShouldEventuallyReturn(t, numServersInSd, numRequiredServersInSd())
		})
	}
}

func TestParallelGetter(t *testing.T) {
	c, cli := helpers.GetTestEtcd(t)
	defer c.Terminate(t)

	serversToAdd := []*Server{
		NewServer("frontend-1", "type1", true, nil),
		NewServer("frontend-2", "type2", true, nil),
		NewServer("frontend-3", "type3", true, nil),
		NewServer("frontend-4", "type4", true, nil),
		NewServer("frontend-5", "type5", true, nil),
		NewServer("frontend-6", "type6", true, nil),
		NewServer("frontend-7", "type7", true, nil),
		NewServer("frontend-8", "type8", true, nil),
		NewServer("frontend-9", "type9", true, nil),
	}

	// Add server
	for _, serverToAdd := range serversToAdd {
		_, err := cli.Put(
			context.TODO(),
			getKey(serverToAdd.ID, serverToAdd.Type),
			serverToAdd.AsJSONString(),
		)
		assert.NoError(t, err)
	}

	parallelGetter := newParallelGetter(cli, 5)
	for _, serverToAdd := range serversToAdd {
		payload := []byte("{\"id\":\"" + serverToAdd.ID + "\",\"type\":\"" + serverToAdd.Type + "\",\"frontend\":true,\"hostname\":\"" + serverToAdd.ID + "\",\"metadata\":{\"region\":\"us\"}}")
		parallelGetter.addWorkWithPayload(serverToAdd.Type, serverToAdd.ID, payload)
	}

	servers := parallelGetter.waitAndGetResult()
	assert.Equal(t, len(serversToAdd), len(servers))

	// We add the returned servers to a map first, since the results may be out the order they were added.
	serversMap := map[string]*Server{}
	for _, sv := range servers {
		_, ok := serversMap[sv.ID]
		assert.False(t, ok)
		serversMap[sv.ID] = sv
	}

	for _, serverToAdd := range serversToAdd {
		sv, ok := serversMap[serverToAdd.ID]
		assert.True(t, ok)
		assert.Equal(t, serverToAdd.Type, sv.Type)
		assert.Equal(t, serverToAdd.ID, sv.ID)
		assert.Equal(t, serverToAdd.Frontend, sv.Frontend)
	}
}
