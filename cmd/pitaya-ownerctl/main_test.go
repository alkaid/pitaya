package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/internal/statefulowner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestRunTopLevelHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			assert.Zero(t, run([]string{arg}, &stdout, &stderr))
			assert.Contains(t, stdout.String(), "usage: pitaya-ownerctl")
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRunPruneStaleWorkflow(t *testing.T) {
	ctx, cli, endpoint := ownerctlTestEtcd(t)
	const prefix = "ownerctl-test/"

	staleKey := ownerctlPutAssignment(t, ctx, cli, prefix, "stale", statefulowner.Record{
		ServerType: "room",
		ServerID:   "missing",
		OwnerEpoch: 99,
	})
	liveRegistration := ownerctlPutRegistration(t, ctx, cli, prefix, "room", "live")
	liveKey := ownerctlPutAssignment(t, ctx, cli, prefix, "live", statefulowner.Record{
		ServerType: "room",
		ServerID:   "live",
		OwnerEpoch: uint64(liveRegistration.Kvs[0].CreateRevision),
	})
	corruptKey, err := statefulowner.FullAssignmentKey(prefix, ownerctlOpaque("corrupt"))
	require.NoError(t, err)
	_, err = cli.Put(ctx, corruptKey, "not-json")
	require.NoError(t, err)

	dryRun, stderr, code := runOwnerctl(t, endpoint, prefix, "prune-stale")
	require.Zero(t, code, stderr)
	assert.True(t, dryRun.DryRun)
	assert.Equal(t, map[statefulowner.Status]int{
		statefulowner.StatusLive:    1,
		statefulowner.StatusStale:   1,
		statefulowner.StatusCorrupt: 1,
	}, dryRun.Counts)
	require.Len(t, dryRun.Cleanup, 1)
	assert.Equal(t, statefulowner.CleanupDryRun, dryRun.Cleanup[0].Status)
	ownerctlRequireKey(t, ctx, cli, staleKey, true)

	_, stderr, code = runOwnerctl(t, endpoint, prefix, "prune-stale", "--apply")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "corrupt assignment")
	ownerctlRequireKey(t, ctx, cli, staleKey, true)

	_, err = cli.Delete(ctx, corruptKey)
	require.NoError(t, err)
	applied, stderr, code := runOwnerctl(t, endpoint, prefix, "prune-stale", "--apply")
	require.Zero(t, code, stderr)
	require.Len(t, applied.Cleanup, 1)
	assert.Equal(t, statefulowner.CleanupDeleted, applied.Cleanup[0].Status)
	ownerctlRequireKey(t, ctx, cli, staleKey, false)
	ownerctlRequireKey(t, ctx, cli, liveKey, true)
}

func TestRunPruneListRejectsLiveAssignment(t *testing.T) {
	ctx, cli, endpoint := ownerctlTestEtcd(t)
	const prefix = "ownerctl-list-test/"
	liveRegistration := ownerctlPutRegistration(t, ctx, cli, prefix, "room", "live")
	liveKey := ownerctlPutAssignment(t, ctx, cli, prefix, "live", statefulowner.Record{
		ServerType: "room",
		ServerID:   "live",
		OwnerEpoch: uint64(liveRegistration.Kvs[0].CreateRevision),
	})
	input := filepath.Join(t.TempDir(), "assignments.txt")
	require.NoError(t, os.WriteFile(input, []byte(liveKey+"\n"), 0o600))

	for _, apply := range []bool{false, true} {
		name := "dry-run"
		extra := []string{"--input", input}
		if apply {
			name = "apply"
			extra = append(extra, "--apply")
		}
		t.Run(name, func(t *testing.T) {
			_, stderr, code := runOwnerctl(t, endpoint, prefix, "prune-list", extra...)
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, "only stale assignments may be pruned")
			ownerctlRequireKey(t, ctx, cli, liveKey, true)
		})
	}
}

func TestRunRejectsExpiredTimeout(t *testing.T) {
	_, _, endpoint := ownerctlTestEtcd(t)
	_, stderr, code := runOwnerctl(t, endpoint, "ownerctl-timeout-test/", "inventory", "--timeout", "1ns")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "context deadline exceeded")
}

func TestFinishReportPreservesPartialCleanupOnError(t *testing.T) {
	const assignmentKey = "pitaya/stateful-owners/v1/partial"
	result := report{
		SchemaVersion: reportSchemaVersion,
		Operation:     "prune-stale",
		Assignments: []statefulowner.Snapshot{{
			AssignmentKey: assignmentKey,
			ServerType:    "room",
			Status:        statefulowner.StatusStale,
		}},
		Cleanup: []statefulowner.CleanupResult{{
			AssignmentKey: assignmentKey,
			Status:        statefulowner.CleanupDeleted,
		}},
	}
	operationErr := errors.New("delete next assignment: etcd unavailable")
	var stdout, stderr bytes.Buffer

	assert.Equal(t, 1, finishReport(&stdout, &stderr, "json", "prune-stale", result, operationErr))

	var emitted report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &emitted))
	assert.Equal(t, operationErr.Error(), emitted.Error)
	require.Len(t, emitted.Cleanup, 1)
	assert.Equal(t, statefulowner.CleanupDeleted, emitted.Cleanup[0].Status)
	require.Len(t, emitted.Metrics.CleanupResults, 1)
	assert.Equal(t, "room", emitted.Metrics.CleanupResults[0].ServerType)
	assert.Equal(t, string(statefulowner.CleanupDeleted), emitted.Metrics.CleanupResults[0].Result)
	assert.Equal(t, int64(1), emitted.Metrics.CleanupResults[0].Count)
	assert.Contains(t, stderr.String(), operationErr.Error())
}

func TestRunRejectsInvalidEnvironment(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "PITAYA_OWNERCTL_TIMEOUT", value: "soon", want: "must be a duration"},
		{name: "PITAYA_OWNERCTL_TLS", value: "sometimes", want: "must be a boolean"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.name, test.value)
			var stdout, stderr bytes.Buffer
			assert.Equal(t, 2, run([]string{"inventory"}, &stdout, &stderr))
			assert.Contains(t, stderr.String(), test.want)
		})
	}
}

func ownerctlTestEtcd(t *testing.T) (context.Context, *clientv3.Client, string) {
	t.Helper()
	embedded, cli := helpers.GetTestEtcd(t)
	t.Cleanup(func() { embedded.Terminate(t) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx, cli, cli.Endpoints()[0]
}

func ownerctlOpaque(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:])
}

func ownerctlPutRegistration(
	t *testing.T,
	ctx context.Context,
	cli *clientv3.Client,
	prefix, serverType, serverID string,
) *clientv3.GetResponse {
	t.Helper()
	key, err := statefulowner.FullRegistrationKey(prefix, statefulowner.Record{
		ServerType: serverType,
		ServerID:   serverID,
	})
	require.NoError(t, err)
	_, err = cli.Put(ctx, key, `{"id":"`+serverID+`"}`)
	require.NoError(t, err)
	response, err := cli.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	return response
}

func ownerctlPutAssignment(
	t *testing.T,
	ctx context.Context,
	cli *clientv3.Client,
	prefix, name string,
	record statefulowner.Record,
) string {
	t.Helper()
	record.Version = statefulowner.RecordVersion
	record.AssignmentKey = ownerctlOpaque(name)
	key, err := statefulowner.FullAssignmentKey(prefix, record.AssignmentKey)
	require.NoError(t, err)
	payload, err := statefulowner.Encode(record)
	require.NoError(t, err)
	_, err = cli.Put(ctx, key, string(payload))
	require.NoError(t, err)
	return key
}

func runOwnerctl(
	t *testing.T,
	endpoint, prefix, operation string,
	extra ...string,
) (report, string, int) {
	t.Helper()
	args := []string{
		operation,
		"--endpoints", endpoint,
		"--prefix", prefix,
		"--format", "json",
		"--dial-timeout", "5s",
		"--timeout", "5s",
	}
	args = append(args, extra...)
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	var result report
	if stdout.Len() > 0 {
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &result), stdout.String())
	}
	return result, stderr.String(), code
}

func ownerctlRequireKey(t *testing.T, ctx context.Context, cli *clientv3.Client, key string, exists bool) {
	t.Helper()
	response, err := cli.Get(ctx, key)
	require.NoError(t, err)
	if exists {
		require.Len(t, response.Kvs, 1)
		return
	}
	require.Empty(t, response.Kvs)
}
