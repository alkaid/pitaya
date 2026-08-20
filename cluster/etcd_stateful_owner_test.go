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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const statefulOwnerTestLeaseTTL = 60

func newStatefulOwnerTestEtcd(t *testing.T) (context.Context, *clientv3.Client) {
	t.Helper()
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx, cli
}

func putStatefulOwnerTestRegistration(
	t *testing.T,
	ctx context.Context,
	cli *clientv3.Client,
	server *Server,
) (clientv3.LeaseID, uint64) {
	t.Helper()
	lease, err := cli.Grant(ctx, statefulOwnerTestLeaseTTL)
	require.NoError(t, err)
	_, err = cli.Put(ctx, getKey(server.ID, server.Type), server.AsJSONString(), clientv3.WithLease(lease.ID))
	require.NoError(t, err)

	response, err := cli.Get(ctx, getKey(server.ID, server.Type))
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	require.Positive(t, response.Kvs[0].CreateRevision)
	epoch := uint64(response.Kvs[0].CreateRevision)
	server.SetOwnerEpoch(epoch)
	return lease.ID, epoch
}

func TestStatefulOwnerAssignmentKeyIsDeterministicAndExact(t *testing.T) {
	const (
		serverType = "room:primary"
		routingKey = "user:42"
	)

	opaque, storageKey, err := statefulOwnerAssignmentKey(serverType, routingKey)
	require.NoError(t, err)
	repeatedOpaque, repeatedStorageKey, err := statefulOwnerAssignmentKey(serverType, routingKey)
	require.NoError(t, err)
	assert.Equal(t, opaque, repeatedOpaque)
	assert.Equal(t, storageKey, repeatedStorageKey)

	structuredDigest := sha256.Sum256([]byte(fmt.Sprintf(
		"%d:%s%d:%s",
		len(serverType), serverType,
		len(routingKey), routingKey,
	)))
	assert.Equal(t, hex.EncodeToString(structuredDigest[:]), opaque)
	assert.Len(t, opaque, sha256.Size*2)
	assert.Equal(t, statefulOwnerKeyPrefix+opaque, storageKey)

	otherTypeOpaque, _, err := statefulOwnerAssignmentKey("room", routingKey)
	require.NoError(t, err)
	otherRoutingOpaque, _, err := statefulOwnerAssignmentKey(serverType, "user:43")
	require.NoError(t, err)
	assert.NotEqual(t, opaque, otherTypeOpaque, "server type must be part of the assignment identity")
	assert.NotEqual(t, opaque, otherRoutingOpaque, "the complete routing key must be part of the assignment identity")
	ambiguousFirst, err := StatefulOwnerAssignmentKey("room:1", "23")
	require.NoError(t, err)
	ambiguousSecond, err := StatefulOwnerAssignmentKey("room", "123")
	require.NoError(t, err)
	assert.NotEqual(
		t,
		ambiguousFirst,
		ambiguousSecond,
		"length framing must distinguish otherwise ambiguous identities",
	)

	for _, args := range []struct {
		serverType string
		routingKey string
	}{
		{serverType: "", routingKey: routingKey},
		{serverType: serverType, routingKey: ""},
	} {
		_, _, err := statefulOwnerAssignmentKey(args.serverType, args.routingKey)
		assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentUnavailable)
	}
}

func TestStatefulOwnerRoutingKeySeparatesUIDAndFrontendSessionDomains(t *testing.T) {
	uidIdentity, err := StatefulOwnerRoutingKey("frontend-1", "", 0)
	require.NoError(t, err)
	sessionIdentity, err := StatefulOwnerRoutingKey("", "frontend", 1)
	require.NoError(t, err)

	assert.Equal(t, "v1:uid:10:frontend-1", uidIdentity)
	assert.Equal(t, "v1:session:8:frontend:1", sessionIdentity)
	assert.NotEqual(t, uidIdentity, sessionIdentity)
	uidKey, err := StatefulOwnerAssignmentKey("room", uidIdentity)
	require.NoError(t, err)
	sessionKey, err := StatefulOwnerAssignmentKey("room", sessionIdentity)
	require.NoError(t, err)
	assert.NotEqual(t, uidKey, sessionKey)
}

func TestStatefulOwnerAssignmentCreation(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	server := NewServer("room-a", "room", false, map[string]string{"zone": "east"})
	_, registrationEpoch := putStatefulOwnerTestRegistration(t, ctx, cli, server)
	sd := &etcdServiceDiscovery{cli: cli}

	owner, fence, err := sd.ResolveStatefulOwner(ctx, server.Type, "user-1", server.ID)
	require.NoError(t, err)
	require.NotNil(t, owner)
	assert.Equal(t, server.ID, owner.ID)
	assert.Equal(t, registrationEpoch, owner.OwnerEpoch())
	assert.Equal(t, server.ID, fence.ServerID)
	assert.NotEmpty(t, fence.AssignmentKey)
	assert.NotZero(t, fence.Epoch)
	requestFence, ok := owner.StatefulOwnerFenceSnapshot()
	require.True(t, ok)
	assert.Equal(t, fence, requestFence)
	_, sharedServerWasMutated := server.StatefulOwnerFenceSnapshot()
	assert.False(t, sharedServerWasMutated)

	opaque, storageKey, err := statefulOwnerAssignmentKey(server.Type, "user-1")
	require.NoError(t, err)
	assert.Equal(t, opaque, fence.AssignmentKey)
	response, err := cli.Get(ctx, storageKey)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	assert.Equal(t, int64(0), response.Kvs[0].Lease, "assignments must outlive registration leases")
	assert.Equal(t, fence.Epoch, uint64(response.Kvs[0].ModRevision))
	record, err := decodeStatefulOwnerRecord(response.Kvs[0].Value)
	require.NoError(t, err)
	assert.Equal(t, statefulOwnerRecord{
		Version:       statefulOwnerRecordVersion,
		ServerType:    server.Type,
		AssignmentKey: opaque,
		ServerID:      server.ID,
		OwnerEpoch:    registrationEpoch,
	}, record)
}

func TestStatefulOwnerConcurrentClaimsConverge(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	first := NewServer("room-a", "room", false)
	second := NewServer("room-b", "room", false)
	putStatefulOwnerTestRegistration(t, ctx, cli, first)
	putStatefulOwnerTestRegistration(t, ctx, cli, second)
	sd := &etcdServiceDiscovery{cli: cli}

	type result struct {
		owner *Server
		fence StatefulOwnerFence
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, candidate := range []string{first.ID, second.ID} {
		candidate := candidate
		go func() {
			<-start
			owner, fence, err := sd.ResolveStatefulOwner(ctx, first.Type, "contended-user", candidate)
			results <- result{owner: owner, fence: fence, err: err}
		}()
	}
	close(start)

	resolved := []result{<-results, <-results}
	for _, actual := range resolved {
		require.NoError(t, actual.err)
		require.NotNil(t, actual.owner)
		assert.Equal(t, actual.fence.ServerID, actual.owner.ID)
		ownerFence, ok := actual.owner.StatefulOwnerFenceSnapshot()
		require.True(t, ok)
		assert.Equal(t, actual.fence, ownerFence)
	}
	assert.Contains(t, []string{first.ID, second.ID}, resolved[0].fence.ServerID)
	assert.Equal(t, resolved[0].fence, resolved[1].fence)
}

func TestStatefulOwnerLiveAssignmentRemainsSticky(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	first := NewServer("room-a", "room", false)
	second := NewServer("room-b", "room", false)
	putStatefulOwnerTestRegistration(t, ctx, cli, first)
	putStatefulOwnerTestRegistration(t, ctx, cli, second)
	sd := &etcdServiceDiscovery{cli: cli}

	firstOwner, firstFence, err := sd.ResolveStatefulOwner(ctx, first.Type, "sticky-user", first.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, firstOwner.ID)
	secondOwner, secondFence, err := sd.ResolveStatefulOwner(ctx, first.Type, "sticky-user", second.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, secondOwner.ID)
	assert.Equal(t, firstFence, secondFence, "a live assignment must survive a changed hash-ring candidate")
}

func TestStatefulOwnerReassignsAfterRegistrationDeletionAndReplacement(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	first := NewServer("room-a", "room", false)
	second := NewServer("room-b", "room", false)
	putStatefulOwnerTestRegistration(t, ctx, cli, first)
	_, firstSecondEpoch := putStatefulOwnerTestRegistration(t, ctx, cli, second)
	sd := &etcdServiceDiscovery{cli: cli}

	_, initialFence, err := sd.ResolveStatefulOwner(ctx, first.Type, "moving-user", first.ID)
	require.NoError(t, err)
	_, err = cli.Delete(ctx, getKey(first.ID, first.Type))
	require.NoError(t, err)

	ownerAfterDeletion, deletionFence, err := sd.ResolveStatefulOwner(ctx, first.Type, "moving-user", second.ID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, ownerAfterDeletion.ID)
	assert.Equal(t, initialFence.AssignmentKey, deletionFence.AssignmentKey)
	assert.Greater(t, deletionFence.Epoch, initialFence.Epoch)

	_, err = cli.Delete(ctx, getKey(second.ID, second.Type))
	require.NoError(t, err)
	_, replacementEpoch := putStatefulOwnerTestRegistration(t, ctx, cli, second)
	require.Greater(t, replacementEpoch, firstSecondEpoch)
	ownerAfterReplacement, replacementFence, err := sd.ResolveStatefulOwner(ctx, second.Type, "moving-user", second.ID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, ownerAfterReplacement.ID)
	assert.Equal(t, deletionFence.AssignmentKey, replacementFence.AssignmentKey)
	assert.Greater(t, replacementFence.Epoch, deletionFence.Epoch)
	assert.Equal(t, replacementEpoch, ownerAfterReplacement.OwnerEpoch())

	assignmentKey, err := statefulOwnerStorageKey(replacementFence.AssignmentKey)
	require.NoError(t, err)
	response, err := cli.Get(ctx, assignmentKey)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	record, err := decodeStatefulOwnerRecord(response.Kvs[0].Value)
	require.NoError(t, err)
	assert.Equal(t, replacementEpoch, record.OwnerEpoch)
}

func TestStatefulOwnerHandoffFencesOldOwner(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	oldServer := NewServer("room-a", "room", false)
	newServer := NewServer("room-b", "room", false)
	putStatefulOwnerTestRegistration(t, ctx, cli, oldServer)
	putStatefulOwnerTestRegistration(t, ctx, cli, newServer)
	oldSD := &etcdServiceDiscovery{cli: cli, server: oldServer}
	newSD := &etcdServiceDiscovery{cli: cli, server: newServer}
	const routingKey = "handoff-user"

	_, oldFence, err := oldSD.ResolveStatefulOwner(ctx, oldServer.Type, routingKey, oldServer.ID)
	require.NoError(t, err)
	require.NoError(t, oldSD.ValidateStatefulOwner(ctx, oldServer.Type, routingKey, oldFence))

	newOwner, newFence, err := oldSD.HandoffStatefulOwner(ctx, oldServer.Type, oldFence, newServer.ID)
	require.NoError(t, err)
	require.NotNil(t, newOwner)
	assert.Equal(t, newServer.ID, newOwner.ID)
	assert.Equal(t, oldFence.AssignmentKey, newFence.AssignmentKey)
	assert.Greater(t, newFence.Epoch, oldFence.Epoch)
	assert.Equal(t, newServer.ID, newFence.ServerID)
	assert.ErrorIs(t, oldSD.ValidateStatefulOwner(ctx, oldServer.Type, routingKey, oldFence), constants.ErrStaleStatefulOwner)
	assert.NoError(t, newSD.ValidateStatefulOwner(ctx, newServer.Type, routingKey, newFence))

	resolved, resolvedFence, err := oldSD.ResolveStatefulOwner(ctx, oldServer.Type, routingKey, oldServer.ID)
	require.NoError(t, err)
	assert.Equal(t, newServer.ID, resolved.ID)
	assert.Equal(t, newFence, resolvedFence)
}

func TestStatefulOwnerCorruptRecordsFailClosed(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	server := NewServer("room-a", "room", false)
	_, registrationEpoch := putStatefulOwnerTestRegistration(t, ctx, cli, server)
	sd := &etcdServiceDiscovery{cli: cli, server: server}
	opaque, assignmentKey, err := statefulOwnerAssignmentKey(server.Type, "corrupt-user")
	require.NoError(t, err)
	wrongOpaque, _, err := statefulOwnerAssignmentKey(server.Type, "different-user")
	require.NoError(t, err)
	identityMismatch, err := encodeStatefulOwnerRecord(statefulOwnerRecord{
		Version:       statefulOwnerRecordVersion,
		ServerType:    server.Type,
		AssignmentKey: wrongOpaque,
		ServerID:      server.ID,
		OwnerEpoch:    registrationEpoch,
	})
	require.NoError(t, err)

	tests := []struct {
		name                string
		payload             string
		validationErrorKind error
	}{
		{
			name:                "malformed json",
			payload:             `{not-json`,
			validationErrorKind: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name: "unknown field",
			payload: fmt.Sprintf(
				`{"version":1,"server_type":%q,"assignment_key":%q,"server_id":%q,"owner_epoch":%d,"extra":true}`,
				server.Type,
				opaque,
				server.ID,
				registrationEpoch,
			),
			validationErrorKind: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name:                "identity mismatch",
			payload:             string(identityMismatch),
			validationErrorKind: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := cli.Put(ctx, assignmentKey, test.payload)
			require.NoError(t, err)
			response, err := cli.Get(ctx, assignmentKey)
			require.NoError(t, err)
			require.Len(t, response.Kvs, 1)

			owner, fence, err := sd.ResolveStatefulOwner(ctx, server.Type, "corrupt-user", server.ID)
			assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentCorrupt)
			assert.Nil(t, owner)
			assert.Equal(t, StatefulOwnerFence{}, fence)
			admissionFence := StatefulOwnerFence{
				AssignmentKey: opaque,
				Epoch:         uint64(response.Kvs[0].ModRevision),
				ServerID:      server.ID,
			}
			assert.ErrorIs(t, sd.ValidateStatefulOwner(ctx, server.Type, "corrupt-user", admissionFence), test.validationErrorKind)

			unchanged, err := cli.Get(ctx, assignmentKey)
			require.NoError(t, err)
			require.Len(t, unchanged.Kvs, 1)
			assert.Equal(t, test.payload, string(unchanged.Kvs[0].Value), "corrupt state must not be silently replaced")
		})
	}
}

func TestStatefulOwnerAssignmentsAreIndependentPerRoutingKey(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	first := NewServer("room-a", "room", false)
	second := NewServer("room-b", "room", false)
	putStatefulOwnerTestRegistration(t, ctx, cli, first)
	putStatefulOwnerTestRegistration(t, ctx, cli, second)
	sd := &etcdServiceDiscovery{cli: cli}

	firstOwner, firstFence, err := sd.ResolveStatefulOwner(ctx, first.Type, "user-a", first.ID)
	require.NoError(t, err)
	secondOwner, secondFence, err := sd.ResolveStatefulOwner(ctx, second.Type, "user-b", second.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, firstOwner.ID)
	assert.Equal(t, second.ID, secondOwner.ID)
	assert.NotEqual(t, firstFence.AssignmentKey, secondFence.AssignmentKey)

	stickyFirst, stickyFence, err := sd.ResolveStatefulOwner(ctx, first.Type, "user-a", second.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, stickyFirst.ID)
	assert.Equal(t, firstFence, stickyFence, "a different routing key's assignment must not replace this one")

	response, err := cli.Get(ctx, statefulOwnerKeyPrefix, clientv3.WithPrefix())
	require.NoError(t, err)
	assert.Len(t, response.Kvs, 2, "each exact routing identity must create its own assignment")
}

func TestStatefulOwnerValidationRejectsCrossRoutingKeyReplay(t *testing.T) {
	ctx, cli := newStatefulOwnerTestEtcd(t)
	server := NewServer("room-a", "room", false)
	putStatefulOwnerTestRegistration(t, ctx, cli, server)
	sd := &etcdServiceDiscovery{cli: cli, server: server}

	_, firstFence, err := sd.ResolveStatefulOwner(ctx, server.Type, "user-a", server.ID)
	require.NoError(t, err)
	_, secondFence, err := sd.ResolveStatefulOwner(ctx, server.Type, "user-b", server.ID)
	require.NoError(t, err)
	require.NotEqual(t, firstFence.AssignmentKey, secondFence.AssignmentKey)

	assert.NoError(t, sd.ValidateStatefulOwner(ctx, server.Type, "user-a", firstFence))
	assert.NoError(t, sd.ValidateStatefulOwner(ctx, server.Type, "user-b", secondFence))
	assert.ErrorIs(
		t,
		sd.ValidateStatefulOwner(ctx, server.Type, "user-b", firstFence),
		constants.ErrStaleStatefulOwner,
	)
	assert.ErrorIs(
		t,
		sd.ValidateStatefulOwner(ctx, server.Type, "user-a", secondFence),
		constants.ErrStaleStatefulOwner,
	)
}
