package statefulowner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func ownerctlTestEtcd(t *testing.T) (context.Context, *clientv3.Client) {
	t.Helper()
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx, cli
}

func testOpaque(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:])
}

func putRegistration(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix, serverType, serverID string) *clientv3.GetResponse {
	t.Helper()
	key, err := FullRegistrationKey(prefix, Record{ServerType: serverType, ServerID: serverID})
	require.NoError(t, err)
	_, err = cli.Put(ctx, key, `{"id":"`+serverID+`"}`)
	require.NoError(t, err)
	response, err := cli.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	return response
}

func putAssignment(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix, name string, record Record) string {
	t.Helper()
	record.Version = RecordVersion
	record.AssignmentKey = testOpaque(name)
	key, err := FullAssignmentKey(prefix, record.AssignmentKey)
	require.NoError(t, err)
	payload, err := Encode(record)
	require.NoError(t, err)
	_, err = cli.Put(ctx, key, string(payload))
	require.NoError(t, err)
	return key
}

func TestInventoryAndPruneClassifyOwnerGenerations(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"

	liveRegistration := putRegistration(t, ctx, cli, prefix, "room", "live")
	liveKey := putAssignment(t, ctx, cli, prefix, "live", Record{
		ServerType: "room",
		ServerID:   "live",
		OwnerEpoch: uint64(liveRegistration.Kvs[0].CreateRevision),
	})

	missingKey := putAssignment(t, ctx, cli, prefix, "missing", Record{
		ServerType: "room",
		ServerID:   "missing",
		OwnerEpoch: 123,
	})

	replacedRegistration := putRegistration(t, ctx, cli, prefix, "room", "replaced")
	replacedEpoch := uint64(replacedRegistration.Kvs[0].CreateRevision)
	replacedRegistrationKey := string(replacedRegistration.Kvs[0].Key)
	_, err := cli.Delete(ctx, replacedRegistrationKey)
	require.NoError(t, err)
	putRegistration(t, ctx, cli, prefix, "room", "replaced")
	replacedKey := putAssignment(t, ctx, cli, prefix, "replaced", Record{
		ServerType: "room",
		ServerID:   "replaced",
		OwnerEpoch: replacedEpoch,
	})

	corruptKey, err := FullAssignmentKey(prefix, testOpaque("corrupt"))
	require.NoError(t, err)
	_, err = cli.Put(ctx, corruptKey, "not-json")
	require.NoError(t, err)

	inventory, err := InventoryAll(ctx, cli, prefix, 10, 1)
	require.NoError(t, err)
	assert.Equal(t, map[Status]int{StatusLive: 1, StatusStale: 2, StatusCorrupt: 1}, inventory.Counts)
	assert.Len(t, inventory.Assignments, 4)
	for _, snapshot := range inventory.Assignments {
		assert.Positive(t, snapshot.ValidationDuration)
	}

	stale := make([]Snapshot, 0, 2)
	for _, snapshot := range inventory.Assignments {
		if snapshot.Status == StatusStale {
			stale = append(stale, snapshot)
		}
	}
	dryRun, err := Prune(ctx, cli, prefix, stale, false)
	require.NoError(t, err)
	require.Len(t, dryRun, 2)
	for _, result := range dryRun {
		assert.Equal(t, CleanupDryRun, result.Status)
	}

	deleted, err := Prune(ctx, cli, prefix, stale, true)
	require.NoError(t, err)
	require.Len(t, deleted, 2)
	for _, result := range deleted {
		assert.Equal(t, CleanupDeleted, result.Status)
	}
	for _, key := range []string{missingKey, replacedKey} {
		response, getErr := cli.Get(ctx, key)
		require.NoError(t, getErr)
		assert.Empty(t, response.Kvs)
	}
	for _, key := range []string{liveKey, corruptKey} {
		response, getErr := cli.Get(ctx, key)
		require.NoError(t, getErr)
		assert.Len(t, response.Kvs, 1)
	}
}

func TestPruneCASRejectsChangedEvidence(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, context.Context, *clientv3.Client, string) string
		change func(*testing.T, context.Context, *clientv3.Client, string, Snapshot)
	}{
		{
			name: "missing registration becomes live",
			setup: func(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix string) string {
				return putAssignment(t, ctx, cli, prefix, "missing-race", Record{
					ServerType: "room", ServerID: "owner", OwnerEpoch: 99,
				})
			},
			change: func(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix string, _ Snapshot) {
				putRegistration(t, ctx, cli, prefix, "room", "owner")
			},
		},
		{
			name: "replacement registration changes",
			setup: func(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix string) string {
				old := putRegistration(t, ctx, cli, prefix, "room", "owner")
				key := string(old.Kvs[0].Key)
				_, err := cli.Delete(ctx, key)
				require.NoError(t, err)
				putRegistration(t, ctx, cli, prefix, "room", "owner")
				return putAssignment(t, ctx, cli, prefix, "replacement-race", Record{
					ServerType: "room", ServerID: "owner", OwnerEpoch: uint64(old.Kvs[0].CreateRevision),
				})
			},
			change: func(t *testing.T, ctx context.Context, cli *clientv3.Client, _ string, snapshot Snapshot) {
				_, err := cli.Put(ctx, snapshot.RegistrationKey, `{"id":"owner","changed":true}`)
				require.NoError(t, err)
			},
		},
		{
			name: "replacement registration becomes matching generation",
			setup: func(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix string) string {
				old := putRegistration(t, ctx, cli, prefix, "room", "owner")
				return putAssignment(t, ctx, cli, prefix, "replacement-matches", Record{
					ServerType: "room",
					ServerID:   "owner",
					OwnerEpoch: uint64(old.Kvs[0].CreateRevision + 3),
				})
			},
			change: func(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix string, snapshot Snapshot) {
				_, err := cli.Delete(ctx, snapshot.RegistrationKey)
				require.NoError(t, err)
				current := putRegistration(t, ctx, cli, prefix, "room", "owner")
				require.Equal(t, snapshot.OwnerEpoch, uint64(current.Kvs[0].CreateRevision))
			},
		},
		{
			name: "assignment changes",
			setup: func(t *testing.T, ctx context.Context, cli *clientv3.Client, prefix string) string {
				return putAssignment(t, ctx, cli, prefix, "assignment-race", Record{
					ServerType: "room", ServerID: "owner", OwnerEpoch: 99,
				})
			},
			change: func(t *testing.T, ctx context.Context, cli *clientv3.Client, _ string, snapshot Snapshot) {
				_, err := cli.Put(ctx, snapshot.AssignmentKey, string(snapshot.assignmentValue))
				require.NoError(t, err)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cli := ownerctlTestEtcd(t)
			const prefix = "tenant/"
			key := test.setup(t, ctx, cli, prefix)
			inventory, err := InventoryKeys(ctx, cli, prefix, []string{key}, 1)
			require.NoError(t, err)
			require.Len(t, inventory.Assignments, 1)
			snapshot := inventory.Assignments[0]
			require.Equal(t, StatusStale, snapshot.Status)

			test.change(t, ctx, cli, prefix, snapshot)
			results, err := Prune(ctx, cli, prefix, []Snapshot{snapshot}, true)
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, CleanupConflict, results[0].Status)
			response, err := cli.Get(ctx, key)
			require.NoError(t, err)
			assert.Len(t, response.Kvs, 1)
		})
	}
}

func TestPruneCASRejectsAssignmentValueSnapshotMismatch(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	key := putAssignment(t, ctx, cli, prefix, "assignment-value", Record{
		ServerType: "room", ServerID: "owner", OwnerEpoch: 99,
	})
	inventory, err := InventoryKeys(ctx, cli, prefix, []string{key}, 1)
	require.NoError(t, err)
	require.Len(t, inventory.Assignments, 1)
	snapshot := inventory.Assignments[0]

	// The payload remains a valid record with the same identity, but it is not
	// the exact value read from etcd and therefore cannot authorize deletion.
	snapshot.assignmentValue = append([]byte("\n"), snapshot.assignmentValue...)
	results, err := Prune(ctx, cli, prefix, []Snapshot{snapshot}, true)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, CleanupConflict, results[0].Status)
	response, err := cli.Get(ctx, key)
	require.NoError(t, err)
	assert.Len(t, response.Kvs, 1)
}

func TestPruneValidatesWholeBatchBeforeWriting(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	staleKey := putAssignment(t, ctx, cli, prefix, "stale", Record{
		ServerType: "room", ServerID: "stale", OwnerEpoch: 99,
	})
	liveRegistration := putRegistration(t, ctx, cli, prefix, "room", "live")
	liveKey := putAssignment(t, ctx, cli, prefix, "live-batch", Record{
		ServerType: "room", ServerID: "live", OwnerEpoch: uint64(liveRegistration.Kvs[0].CreateRevision),
	})
	inventory, err := InventoryKeys(ctx, cli, prefix, []string{staleKey, liveKey}, 2)
	require.NoError(t, err)

	_, err = Prune(ctx, cli, prefix, inventory.Assignments, true)
	assert.ErrorContains(t, err, "only stale assignments")
	response, err := cli.Get(ctx, staleKey)
	require.NoError(t, err)
	assert.Len(t, response.Kvs, 1, "batch validation must complete before the first delete")
}

func TestPruneRejectsInvalidWholeBatchBeforeWriting(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	assignmentPrefix, err := AssignmentPrefix(prefix)
	require.NoError(t, err)
	registrationKey, err := FullRegistrationKey(prefix, Record{ServerType: "room", ServerID: "owner"})
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*testing.T, Snapshot) []Snapshot
	}{
		{
			name: "outside prefix",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.AssignmentKey = "other/" + AssignmentRelativePrefix + testOpaque("outside")
				return []Snapshot{snapshot}
			},
		},
		{
			name: "registration key",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.AssignmentKey = registrationKey
				return []Snapshot{snapshot}
			},
		},
		{
			name: "malformed suffix",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.AssignmentKey = assignmentPrefix + "abc"
				return []Snapshot{snapshot}
			},
		},
		{
			name: "uppercase suffix",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.AssignmentKey = assignmentPrefix + strings.ToUpper(testOpaque("uppercase"))
				return []Snapshot{snapshot}
			},
		},
		{
			name: "record identity mismatch",
			mutate: func(t *testing.T, snapshot Snapshot) []Snapshot {
				record, decodeErr := Decode(snapshot.assignmentValue)
				require.NoError(t, decodeErr)
				record.AssignmentKey = testOpaque("different")
				payload, encodeErr := Encode(record)
				require.NoError(t, encodeErr)
				snapshot.assignmentValue = payload
				return []Snapshot{snapshot}
			},
		},
		{
			name: "invalid assignment value",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.assignmentValue = []byte("not-json")
				return []Snapshot{snapshot}
			},
		},
		{
			name: "snapshot identity mismatch",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.ServerType = "connector"
				return []Snapshot{snapshot}
			},
		},
		{
			name: "invalid registration key",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.RegistrationKey += "-changed"
				return []Snapshot{snapshot}
			},
		},
		{
			name: "invalid missing registration evidence",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.RegistrationVersion = 1
				return []Snapshot{snapshot}
			},
		},
		{
			name: "invalid replacement registration evidence",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.StaleReason = StaleReasonReplaced
				return []Snapshot{snapshot}
			},
		},
		{
			name: "missing stale reason",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.StaleReason = ""
				return []Snapshot{snapshot}
			},
		},
		{
			name: "duplicate key",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				return []Snapshot{snapshot, snapshot}
			},
		},
		{
			name: "invalid assignment revision",
			mutate: func(_ *testing.T, snapshot Snapshot) []Snapshot {
				snapshot.AssignmentModRevision = 0
				return []Snapshot{snapshot}
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := putAssignment(t, ctx, cli, prefix, "invalid-batch-"+test.name, Record{
				ServerType: "room", ServerID: "owner", OwnerEpoch: uint64(100 + index),
			})
			inventory, inventoryErr := InventoryKeys(ctx, cli, prefix, []string{key}, 1)
			require.NoError(t, inventoryErr)
			require.Len(t, inventory.Assignments, 1)

			_, pruneErr := Prune(ctx, cli, prefix, test.mutate(t, inventory.Assignments[0]), true)
			assert.Error(t, pruneErr)
			response, getErr := cli.Get(ctx, key)
			require.NoError(t, getErr)
			assert.Len(t, response.Kvs, 1, "batch validation must precede every delete")
		})
	}
}

func TestClassifyPropagatesRegistrationReadError(t *testing.T) {
	_, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	opaque := testOpaque("registration-read-error")
	key, err := FullAssignmentKey(prefix, opaque)
	require.NoError(t, err)
	payload, err := Encode(Record{
		Version:       RecordVersion,
		ServerType:    "room",
		AssignmentKey: opaque,
		ServerID:      "owner",
		OwnerEpoch:    99,
	})
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = classify(canceled, cli, prefix, key, payload, 1)
	assert.ErrorContains(t, err, "read registration")
}

func TestInventoryPaginationLimitAndKeyValidation(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	for _, name := range []string{"a", "b", "c"} {
		putAssignment(t, ctx, cli, prefix, name, Record{
			ServerType: "room", ServerID: name, OwnerEpoch: 99,
		})
	}
	inventory, err := InventoryAll(ctx, cli, prefix, 3, 1)
	require.NoError(t, err)
	assert.Len(t, inventory.Assignments, 3)
	_, err = InventoryAll(ctx, cli, prefix, 2, 1)
	assert.ErrorIs(t, err, ErrLimitExceeded)

	valid, err := FullAssignmentKey(prefix, testOpaque("valid"))
	require.NoError(t, err)
	for _, keys := range [][]string{
		{"other/" + AssignmentRelativePrefix + testOpaque("valid")},
		{valid[:len(valid)-1] + "A"},
		{valid, valid},
	} {
		_, err := InventoryKeys(ctx, cli, prefix, keys, 10)
		assert.Error(t, err)
	}
	_, err = NamespacePrefix("tenant")
	assert.Error(t, err)
	assert.Error(t, ValidateAssignmentKey("ABC"))
}

func TestInventoryBoundaryValidationAndDefaults(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"

	_, err := InventoryAll(ctx, nil, prefix, 1, 1)
	assert.ErrorContains(t, err, "etcd client is required")
	_, err = InventoryAll(ctx, cli, "tenant", 1, 1)
	assert.ErrorContains(t, err, "must end with /")
	_, err = InventoryAll(ctx, cli, prefix, 0, 1)
	assert.ErrorContains(t, err, "limit must be positive")

	for _, pageSize := range []int{0, 2} {
		inventory, inventoryErr := InventoryAll(ctx, cli, prefix, 1, pageSize)
		require.NoError(t, inventoryErr)
		assert.Empty(t, inventory.Assignments)
		assert.Equal(t, map[Status]int{StatusLive: 0, StatusStale: 0, StatusCorrupt: 0}, inventory.Counts)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = InventoryAll(canceled, cli, prefix, 1, 1)
	assert.ErrorContains(t, err, "read assignment inventory")

	_, err = InventoryKeys(ctx, nil, prefix, nil, 1)
	assert.ErrorContains(t, err, "etcd client is required")
	_, err = InventoryKeys(ctx, cli, prefix, nil, 0)
	assert.ErrorIs(t, err, ErrLimitExceeded)
	valid, err := FullAssignmentKey(prefix, testOpaque("missing"))
	require.NoError(t, err)
	_, err = InventoryKeys(ctx, cli, prefix, []string{valid, valid + "0"}, 1)
	assert.ErrorIs(t, err, ErrLimitExceeded)

	inventory, err := InventoryKeys(ctx, cli, prefix, []string{valid}, 1)
	require.NoError(t, err)
	assert.Empty(t, inventory.Assignments)
	assert.Equal(t, map[Status]int{StatusLive: 0, StatusStale: 0, StatusCorrupt: 0}, inventory.Counts)
	_, err = InventoryKeys(canceled, cli, prefix, []string{valid}, 1)
	assert.ErrorContains(t, err, "read assignment")
}

func TestClassifyCorruptAssignmentMetadata(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	opaque := testOpaque("metadata")
	key, err := FullAssignmentKey(prefix, opaque)
	require.NoError(t, err)

	validRecord := Record{
		Version:       RecordVersion,
		ServerType:    "room",
		AssignmentKey: opaque,
		ServerID:      "owner",
		OwnerEpoch:    1,
	}

	tests := []struct {
		name           string
		key            string
		record         Record
		wantDiagnostic string
	}{
		{name: "invalid full key", key: "other/key", record: validRecord, wantDiagnostic: "outside"},
		{
			name: "record identity mismatch",
			key:  key,
			record: Record{
				Version:       RecordVersion,
				ServerType:    "room",
				AssignmentKey: testOpaque("different"),
				ServerID:      "owner",
				OwnerEpoch:    1,
			},
			wantDiagnostic: "identity does not match",
		},
		{
			name: "invalid registration identity",
			key:  key,
			record: Record{
				Version:       RecordVersion,
				ServerType:    "room/invalid",
				AssignmentKey: opaque,
				ServerID:      "owner",
				OwnerEpoch:    1,
			},
			wantDiagnostic: "invalid owner registration identity",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, encodeErr := Encode(test.record)
			require.NoError(t, encodeErr)
			snapshot, classifyErr := classify(ctx, cli, prefix, test.key, payload, 7)
			require.NoError(t, classifyErr)
			assert.Equal(t, StatusCorrupt, snapshot.Status)
			assert.Contains(t, snapshot.Diagnostic, test.wantDiagnostic)
			assert.Positive(t, snapshot.ValidationDuration)
		})
	}
}

func TestPruneBoundaryValidationAndCanceledTransaction(t *testing.T) {
	ctx, cli := ownerctlTestEtcd(t)
	const prefix = "tenant/"
	key := putAssignment(t, ctx, cli, prefix, "prune-canceled", Record{
		ServerType: "room", ServerID: "missing", OwnerEpoch: 99,
	})
	inventory, err := InventoryKeys(ctx, cli, prefix, []string{key}, 1)
	require.NoError(t, err)
	require.Len(t, inventory.Assignments, 1)

	_, err = Prune(ctx, nil, prefix, inventory.Assignments, true)
	assert.ErrorContains(t, err, "etcd client is required")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := Prune(canceled, cli, prefix, inventory.Assignments, true)
	assert.Empty(t, results)
	assert.ErrorContains(t, err, "delete assignment")
}

func TestRecordCodecAndKeyHelpers(t *testing.T) {
	const prefix = "tenant/"
	opaque := testOpaque("codec")
	record := Record{
		Version:       RecordVersion,
		ServerType:    "room",
		AssignmentKey: opaque,
		ServerID:      "owner",
		OwnerEpoch:    42,
	}
	payload, err := Encode(record)
	require.NoError(t, err)
	decoded, err := Decode(payload)
	require.NoError(t, err)
	assert.Equal(t, record, decoded)

	_, err = Decode(append(payload, []byte("{}")...))
	assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentCorrupt)
	invalidPayload, err := Encode(Record{Version: RecordVersion})
	require.NoError(t, err)
	_, err = Decode(invalidPayload)
	assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentCorrupt)

	relative, err := AssignmentRelativeKey(opaque)
	require.NoError(t, err)
	assert.Equal(t, AssignmentRelativePrefix+opaque, relative)
	_, err = AssignmentRelativeKey("not-a-key")
	assert.Error(t, err)
	registration, err := RegistrationRelativeKey("room", "owner")
	require.NoError(t, err)
	assert.Equal(t, "servers/room/owner", registration)
	_, err = RegistrationRelativeKey("room/invalid", "owner")
	assert.Error(t, err)

	namespace, err := NamespacePrefix("")
	require.NoError(t, err)
	assert.Empty(t, namespace)
	_, err = NamespacePrefix(" tenant/")
	assert.Error(t, err)
	assignmentPrefix, err := AssignmentPrefix(prefix)
	require.NoError(t, err)
	assert.Equal(t, prefix+AssignmentRelativePrefix, assignmentPrefix)
	_, err = AssignmentPrefix("tenant")
	assert.Error(t, err)

	fullKey, err := FullAssignmentKey(prefix, opaque)
	require.NoError(t, err)
	assert.Equal(t, assignmentPrefix+opaque, fullKey)
	_, err = FullAssignmentKey("tenant", opaque)
	assert.Error(t, err)
	_, err = FullAssignmentKey(prefix, "not-a-key")
	assert.Error(t, err)
	parsed, err := ParseFullAssignmentKey(prefix, fullKey)
	require.NoError(t, err)
	assert.Equal(t, opaque, parsed)
	_, err = ParseFullAssignmentKey("tenant", fullKey)
	assert.Error(t, err)

	registrationKey, err := FullRegistrationKey(prefix, record)
	require.NoError(t, err)
	assert.Equal(t, prefix+registration, registrationKey)
	_, err = FullRegistrationKey("tenant", record)
	assert.Error(t, err)
	record.ServerID = "invalid/id"
	_, err = FullRegistrationKey(prefix, record)
	assert.Error(t, err)
}

func TestSortSnapshots(t *testing.T) {
	snapshots := []Snapshot{
		{AssignmentKey: "tenant/stateful-owners/v1/c"},
		{AssignmentKey: "tenant/stateful-owners/v1/a"},
		{AssignmentKey: "tenant/stateful-owners/v1/b"},
	}
	SortSnapshots(snapshots)
	assert.Equal(t, []string{
		"tenant/stateful-owners/v1/a",
		"tenant/stateful-owners/v1/b",
		"tenant/stateful-owners/v1/c",
	}, []string{snapshots[0].AssignmentKey, snapshots[1].AssignmentKey, snapshots[2].AssignmentKey})
}
