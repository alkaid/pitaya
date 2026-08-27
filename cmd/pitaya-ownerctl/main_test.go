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
	"strings"
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
			result, stderr, code := runOwnerctl(t, endpoint, prefix, "prune-list", extra...)
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, "only stale assignments may be pruned")
			assert.Contains(t, result.Error, "only stale assignments may be pruned")
			require.Len(t, result.Assignments, 1)
			assert.Equal(t, liveKey, result.Assignments[0].AssignmentKey)
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
		{name: "PITAYA_OWNERCTL_DIAL_TIMEOUT", value: "later", want: "must be a duration"},
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

func TestRunCommandLineValidation(t *testing.T) {
	clearOwnerctlEnv(t)
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantText string
	}{
		{name: "missing operation", wantCode: 2, wantText: "usage: pitaya-ownerctl"},
		{name: "unknown operation", args: []string{"remove"}, wantCode: 2, wantText: "unknown operation"},
		{name: "subcommand help", args: []string{"inventory", "--help"}, wantCode: 0, wantText: "Usage of pitaya-ownerctl inventory"},
		{name: "invalid flag value", args: []string{"inventory", "--timeout", "later"}, wantCode: 2, wantText: "invalid value"},
		{name: "unexpected argument", args: []string{"inventory", "extra"}, wantCode: 2, wantText: "unexpected arguments"},
		{name: "zero timeout", args: []string{"inventory", "--timeout", "0s"}, wantCode: 2, wantText: "must be positive"},
		{name: "zero dial timeout", args: []string{"inventory", "--dial-timeout", "0s"}, wantCode: 2, wantText: "must be positive"},
		{name: "zero limit", args: []string{"inventory", "--limit", "0"}, wantCode: 2, wantText: "limit and page-size"},
		{name: "zero page size", args: []string{"inventory", "--page-size", "0"}, wantCode: 2, wantText: "limit and page-size"},
		{name: "invalid format", args: []string{"inventory", "--format", "yaml"}, wantCode: 2, wantText: "format must be"},
		{name: "invalid prefix", args: []string{"inventory", "--prefix", "tenant"}, wantCode: 2, wantText: "must end with /"},
		{name: "missing prune list input", args: []string{"prune-list"}, wantCode: 2, wantText: "requires --input"},
		{name: "empty endpoint", args: []string{"inventory", "--endpoints", "localhost:2379,"}, wantCode: 2, wantText: "empty values"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			assert.Equal(t, test.wantCode, run(test.args, &stdout, &stderr))
			assert.Contains(t, stdout.String()+stderr.String(), test.wantText)
		})
	}
}

func TestRunReportsTLSConfigurationFailure(t *testing.T) {
	clearOwnerctlEnv(t)
	missingCA := filepath.Join(t.TempDir(), "missing-ca.pem")
	var stdout, stderr bytes.Buffer
	code := run([]string{"inventory", "--tls", "--ca-file", missingCA}, &stdout, &stderr)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "connect to etcd")
}

func TestParseFlagsEnvironmentAndClientEndpoints(t *testing.T) {
	clearOwnerctlEnv(t)
	t.Setenv("PITAYA_OWNERCTL_DIAL_TIMEOUT", "2s")
	t.Setenv("PITAYA_OWNERCTL_TIMEOUT", "3s")
	t.Setenv("PITAYA_OWNERCTL_TLS", "true")
	t.Setenv("PITAYA_OWNERCTL_ENDPOINTS", " first:2379 , second:2379 ")
	t.Setenv("PITAYA_OWNERCTL_PREFIX", "tenant/")
	t.Setenv("PITAYA_OWNERCTL_USER", "owner")
	t.Setenv("PITAYA_OWNERCTL_PASSWORD", "secret")
	var stderr bytes.Buffer
	cfg, err := parseFlags("prune-list", []string{
		"--input", "keys.txt",
		"--format", "json",
		"--apply",
		"--limit", "7",
		"--page-size", "3",
	}, &stderr)
	require.NoError(t, err, stderr.String())
	assert.Equal(t, " first:2379 , second:2379 ", cfg.endpoints)
	assert.Equal(t, "tenant/", cfg.prefix)
	assert.Equal(t, "owner", cfg.user)
	assert.Equal(t, "secret", cfg.password)
	assert.Equal(t, 2*time.Second, cfg.dialTimeout)
	assert.Equal(t, 3*time.Second, cfg.timeout)
	assert.True(t, cfg.tlsEnabled)
	assert.True(t, cfg.apply)
	assert.Equal(t, "keys.txt", cfg.input)
	assert.Equal(t, 7, cfg.limit)
	assert.Equal(t, 3, cfg.pageSize)

	client, err := newClient(commandConfig{
		endpoints:   cfg.endpoints,
		dialTimeout: cfg.dialTimeout,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(t, []string{"first:2379", "second:2379"}, client.Endpoints())

	assert.Equal(t, "fallback", envOr("PITAYA_OWNERCTL_UNSET", "fallback"))
	t.Setenv("PITAYA_OWNERCTL_VALUE", "configured")
	assert.Equal(t, "configured", envOr("PITAYA_OWNERCTL_VALUE", "fallback"))
	duration, err := durationEnv("PITAYA_OWNERCTL_UNSET", time.Second)
	require.NoError(t, err)
	assert.Equal(t, time.Second, duration)
	t.Setenv("PITAYA_OWNERCTL_DURATION", "4s")
	duration, err = durationEnv("PITAYA_OWNERCTL_DURATION", time.Second)
	require.NoError(t, err)
	assert.Equal(t, 4*time.Second, duration)
	enabled, err := boolEnv("PITAYA_OWNERCTL_UNSET")
	require.NoError(t, err)
	assert.False(t, enabled)
	t.Setenv("PITAYA_OWNERCTL_BOOL", "true")
	enabled, err = boolEnv("PITAYA_OWNERCTL_BOOL")
	require.NoError(t, err)
	assert.True(t, enabled)
}

func TestReadKeyListBoundaries(t *testing.T) {
	writeInput := func(t *testing.T, contents string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "keys.txt")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		return path
	}

	t.Run("comments blanks and trim", func(t *testing.T) {
		path := writeInput(t, "\n# reviewed\n  first-key  \nsecond-key\n")
		keys, err := readKeyList(path, 2)
		require.NoError(t, err)
		assert.Equal(t, []string{"first-key", "second-key"}, keys)
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := readKeyList(filepath.Join(t.TempDir(), "missing"), 1)
		assert.ErrorContains(t, err, "open input")
	})
	t.Run("embedded whitespace", func(t *testing.T) {
		_, err := readKeyList(writeInput(t, "two keys\n"), 1)
		assert.ErrorContains(t, err, "must contain one full assignment key")
	})
	t.Run("limit", func(t *testing.T) {
		_, err := readKeyList(writeInput(t, "one\ntwo\n"), 1)
		assert.ErrorIs(t, err, statefulowner.ErrLimitExceeded)
	})
	t.Run("scanner error", func(t *testing.T) {
		_, err := readKeyList(writeInput(t, strings.Repeat("x", 70*1024)), 1)
		assert.ErrorContains(t, err, "read input")
	})
}

type reportErrorWriter struct {
	err error
}

func (w reportErrorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestReportTableMetricsAndWriterErrors(t *testing.T) {
	assignments := []statefulowner.Snapshot{
		{
			AssignmentKey:        "tenant/stateful-owners/v1/b",
			ServerType:           "room",
			OwnerID:              "owner-b",
			OwnerEpoch:           2,
			RegistrationRevision: 20,
			Status:               statefulowner.StatusStale,
			StaleReason:          statefulowner.StaleReasonMissing,
			ValidationDuration:   2 * time.Millisecond,
		},
		{
			AssignmentKey:        "tenant/stateful-owners/v1/a",
			ServerType:           "connector",
			OwnerID:              "owner-a",
			OwnerEpoch:           1,
			RegistrationRevision: 10,
			Status:               statefulowner.StatusLive,
			Diagnostic:           "checked",
			ValidationDuration:   time.Millisecond,
		},
		{
			AssignmentKey:      "tenant/stateful-owners/v1/c",
			Status:             statefulowner.StatusCorrupt,
			ValidationDuration: 3 * time.Millisecond,
		},
	}
	cleanup := []statefulowner.CleanupResult{
		{AssignmentKey: assignments[0].AssignmentKey, Status: statefulowner.CleanupConflict},
		{AssignmentKey: assignments[1].AssignmentKey, Status: statefulowner.CleanupDeleted},
	}
	metrics := buildOwnerMetrics(assignments, cleanup)
	require.Len(t, metrics.InventoryCounts, 3)
	assert.Equal(t, []string{"connector", "room", "unknown"}, []string{
		metrics.InventoryCounts[0].ServerType,
		metrics.InventoryCounts[1].ServerType,
		metrics.InventoryCounts[2].ServerType,
	})
	require.Len(t, metrics.ValidationLatency, 3)
	assert.Equal(t, "connector", metrics.ValidationLatency[0].ServerType)
	assert.Equal(t, "room", metrics.ValidationLatency[1].ServerType)
	assert.Equal(t, "unknown", metrics.ValidationLatency[2].ServerType)
	require.Len(t, metrics.CleanupResults, 2)
	assert.Equal(t, "connector", metrics.CleanupResults[0].ServerType)
	assert.Equal(t, "room", metrics.CleanupResults[1].ServerType)

	result := report{Assignments: assignments, Cleanup: cleanup, Metrics: metrics}
	var table bytes.Buffer
	require.NoError(t, writeReport(&table, "table", result))
	assert.Contains(t, table.String(), "STATUS")
	assert.Contains(t, table.String(), "registration_missing, conflict")
	assert.Contains(t, table.String(), "checked, deleted")

	writeErr := errors.New("output unavailable")
	assert.ErrorIs(t, writeReport(reportErrorWriter{err: writeErr}, "table", result), writeErr)
	var stderr bytes.Buffer
	assert.Equal(t, 1, finishReport(reportErrorWriter{err: writeErr}, &stderr, "json", "inventory", report{}, nil))
	assert.Contains(t, stderr.String(), "write report: output unavailable")
}

func clearOwnerctlEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PITAYA_OWNERCTL_ENDPOINTS",
		"PITAYA_OWNERCTL_USER",
		"PITAYA_OWNERCTL_PASSWORD",
		"PITAYA_OWNERCTL_PREFIX",
		"PITAYA_OWNERCTL_DIAL_TIMEOUT",
		"PITAYA_OWNERCTL_TIMEOUT",
		"PITAYA_OWNERCTL_TLS",
		"PITAYA_OWNERCTL_CA_FILE",
		"PITAYA_OWNERCTL_CERT_FILE",
		"PITAYA_OWNERCTL_KEY_FILE",
		"PITAYA_OWNERCTL_SERVER_NAME",
	} {
		t.Setenv(name, "")
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
