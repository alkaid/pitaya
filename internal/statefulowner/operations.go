package statefulowner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const DefaultPageSize = 256

type Status string

const (
	StatusLive    Status = "live"
	StatusStale   Status = "stale"
	StatusCorrupt Status = "corrupt"
)

type StaleReason string

const (
	StaleReasonMissing  StaleReason = "registration_missing"
	StaleReasonReplaced StaleReason = "registration_replaced"
)

type Snapshot struct {
	AssignmentKey         string        `json:"assignment_key"`
	AssignmentModRevision int64         `json:"assignment_mod_revision"`
	ServerType            string        `json:"server_type,omitempty"`
	OwnerID               string        `json:"owner_id,omitempty"`
	OwnerEpoch            uint64        `json:"owner_epoch,omitempty"`
	RegistrationKey       string        `json:"registration_key,omitempty"`
	RegistrationVersion   int64         `json:"registration_version"`
	RegistrationRevision  int64         `json:"registration_mod_revision"`
	RegistrationDigest    string        `json:"registration_value_digest,omitempty"`
	Status                Status        `json:"status"`
	StaleReason           StaleReason   `json:"stale_reason,omitempty"`
	Diagnostic            string        `json:"diagnostic,omitempty"`
	ValidationDuration    time.Duration `json:"validation_duration"`

	assignmentValue            []byte
	registrationValue          []byte
	registrationCreateRevision int64
}

type Inventory struct {
	Assignments []Snapshot     `json:"assignments"`
	Counts      map[Status]int `json:"counts"`
}

var ErrLimitExceeded = errors.New("stateful owner inventory limit exceeded")

func InventoryAll(ctx context.Context, cli *clientv3.Client, prefix string, limit, pageSize int) (Inventory, error) {
	if cli == nil {
		return Inventory{}, fmt.Errorf("etcd client is required")
	}
	assignmentPrefix, err := AssignmentPrefix(prefix)
	if err != nil {
		return Inventory{}, err
	}
	if limit <= 0 {
		return Inventory{}, fmt.Errorf("limit must be positive")
	}
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > limit {
		pageSize = limit
	}

	kvs, more, err := readAssignmentRange(ctx, cli, assignmentPrefix, limit, pageSize)
	if err != nil {
		return Inventory{}, err
	}
	if more {
		return Inventory{}, fmt.Errorf("%w: found more than %d assignments", ErrLimitExceeded, limit)
	}
	result := Inventory{
		Assignments: make([]Snapshot, 0, len(kvs)),
		Counts:      map[Status]int{StatusLive: 0, StatusStale: 0, StatusCorrupt: 0},
	}
	for _, kv := range kvs {
		snapshot, err := classify(ctx, cli, prefix, string(kv.Key), kv.Value, kv.ModRevision)
		if err != nil {
			return Inventory{}, err
		}
		result.Assignments = append(result.Assignments, snapshot)
		result.Counts[snapshot.Status]++
	}
	return result, nil
}

func InventoryKeys(ctx context.Context, cli *clientv3.Client, prefix string, keys []string, limit int) (Inventory, error) {
	if cli == nil {
		return Inventory{}, fmt.Errorf("etcd client is required")
	}
	if limit <= 0 || len(keys) > limit {
		return Inventory{}, fmt.Errorf("%w: selected %d assignments with limit %d", ErrLimitExceeded, len(keys), limit)
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, err := ParseFullAssignmentKey(prefix, key); err != nil {
			return Inventory{}, err
		}
		if _, duplicate := seen[key]; duplicate {
			return Inventory{}, fmt.Errorf("duplicate assignment key %q", key)
		}
		seen[key] = struct{}{}
	}

	result := Inventory{
		Assignments: make([]Snapshot, 0, len(keys)),
		Counts:      map[Status]int{StatusLive: 0, StatusStale: 0, StatusCorrupt: 0},
	}
	for _, key := range keys {
		response, err := cli.Get(ctx, key)
		if err != nil {
			return Inventory{}, fmt.Errorf("read assignment %q: %w", key, err)
		}
		if len(response.Kvs) == 0 {
			continue
		}
		if len(response.Kvs) != 1 {
			return Inventory{}, fmt.Errorf("assignment %q returned %d values", key, len(response.Kvs))
		}
		kv := response.Kvs[0]
		snapshot, err := classify(ctx, cli, prefix, string(kv.Key), kv.Value, kv.ModRevision)
		if err != nil {
			return Inventory{}, err
		}
		result.Assignments = append(result.Assignments, snapshot)
		result.Counts[snapshot.Status]++
	}
	return result, nil
}

func readAssignmentRange(ctx context.Context, cli *clientv3.Client, prefix string, max, pageSize int) ([]*mvccpb.KeyValue, bool, error) {
	start := prefix
	rangeEnd := clientv3.GetPrefixRangeEnd(prefix)
	result := make([]*mvccpb.KeyValue, 0, min(max, pageSize))
	var revision int64
	var more bool
	for len(result) < max {
		requestLimit := min(pageSize, max-len(result))
		options := []clientv3.OpOption{
			clientv3.WithRange(rangeEnd),
			clientv3.WithLimit(int64(requestLimit)),
			clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
		}
		if revision > 0 {
			options = append(options, clientv3.WithRev(revision))
		}
		response, err := cli.Get(ctx, start, options...)
		if err != nil {
			return nil, false, fmt.Errorf("read assignment inventory: %w", err)
		}
		if revision == 0 && response.Header != nil {
			revision = response.Header.Revision
		}
		result = append(result, response.Kvs...)
		more = response.More
		if !response.More || len(response.Kvs) == 0 {
			break
		}
		start = string(response.Kvs[len(response.Kvs)-1].Key) + "\x00"
	}
	return result, more, nil
}

func classify(ctx context.Context, cli *clientv3.Client, prefix, key string, value []byte, modRevision int64) (snapshot Snapshot, operationalErr error) {
	started := time.Now()
	snapshot = Snapshot{
		AssignmentKey:         key,
		AssignmentModRevision: modRevision,
		Status:                StatusCorrupt,
		assignmentValue:       append([]byte(nil), value...),
	}
	defer func() { snapshot.ValidationDuration = time.Since(started) }()

	opaque, err := ParseFullAssignmentKey(prefix, key)
	if err != nil {
		snapshot.Diagnostic = err.Error()
		return snapshot, nil
	}
	record, err := Decode(value)
	if err != nil {
		snapshot.Diagnostic = err.Error()
		return snapshot, nil
	}
	if record.AssignmentKey != opaque {
		snapshot.Diagnostic = "assignment record identity does not match key suffix"
		return snapshot, nil
	}
	registrationKey, err := FullRegistrationKey(prefix, record)
	if err != nil {
		snapshot.Diagnostic = err.Error()
		return snapshot, nil
	}
	snapshot.ServerType = record.ServerType
	snapshot.OwnerID = record.ServerID
	snapshot.OwnerEpoch = record.OwnerEpoch
	snapshot.RegistrationKey = registrationKey

	registration, err := cli.Get(ctx, registrationKey)
	if err != nil {
		return snapshot, fmt.Errorf("read registration %q: %w", registrationKey, err)
	}
	if len(registration.Kvs) == 0 {
		snapshot.Status = StatusStale
		snapshot.StaleReason = StaleReasonMissing
		return snapshot, nil
	}
	if len(registration.Kvs) != 1 {
		snapshot.Diagnostic = fmt.Sprintf("registration returned %d values", len(registration.Kvs))
		return snapshot, nil
	}
	kv := registration.Kvs[0]
	snapshot.RegistrationVersion = kv.Version
	snapshot.RegistrationRevision = kv.ModRevision
	snapshot.registrationCreateRevision = kv.CreateRevision
	snapshot.registrationValue = append([]byte(nil), kv.Value...)
	digest := sha256.Sum256(kv.Value)
	snapshot.RegistrationDigest = hex.EncodeToString(digest[:])
	if uint64(kv.CreateRevision) != record.OwnerEpoch {
		snapshot.Status = StatusStale
		snapshot.StaleReason = StaleReasonReplaced
		return snapshot, nil
	}
	snapshot.Status = StatusLive
	return snapshot, nil
}

type CleanupStatus string

const (
	CleanupDryRun   CleanupStatus = "dry_run"
	CleanupDeleted  CleanupStatus = "deleted"
	CleanupConflict CleanupStatus = "conflict"
)

type CleanupResult struct {
	AssignmentKey string        `json:"assignment_key"`
	Status        CleanupStatus `json:"status"`
}

func Prune(ctx context.Context, cli *clientv3.Client, prefix string, snapshots []Snapshot, apply bool) ([]CleanupResult, error) {
	if cli == nil {
		return nil, fmt.Errorf("etcd client is required")
	}
	if err := validatePruneBatch(prefix, snapshots); err != nil {
		return nil, err
	}
	results := make([]CleanupResult, 0, len(snapshots))
	for _, snapshot := range snapshots {
		result := CleanupResult{AssignmentKey: snapshot.AssignmentKey, Status: CleanupDryRun}
		if !apply {
			results = append(results, result)
			continue
		}

		comparisons := []clientv3.Cmp{
			clientv3.Compare(clientv3.ModRevision(snapshot.AssignmentKey), "=", snapshot.AssignmentModRevision),
			clientv3.Compare(clientv3.Value(snapshot.AssignmentKey), "=", string(snapshot.assignmentValue)),
		}
		switch snapshot.StaleReason {
		case StaleReasonMissing:
			comparisons = append(comparisons,
				clientv3.Compare(clientv3.Version(snapshot.RegistrationKey), "=", 0),
			)
		case StaleReasonReplaced:
			comparisons = append(comparisons,
				clientv3.Compare(clientv3.ModRevision(snapshot.RegistrationKey), "=", snapshot.RegistrationRevision),
				clientv3.Compare(clientv3.Value(snapshot.RegistrationKey), "=", string(snapshot.registrationValue)),
			)
		}
		transaction, err := cli.Txn(ctx).
			If(comparisons...).
			Then(clientv3.OpDelete(snapshot.AssignmentKey)).
			Commit()
		if err != nil {
			return results, fmt.Errorf("delete assignment %q: %w", snapshot.AssignmentKey, err)
		}
		if transaction.Succeeded {
			result.Status = CleanupDeleted
		} else {
			result.Status = CleanupConflict
		}
		results = append(results, result)
	}
	return results, nil
}

func validatePruneBatch(prefix string, snapshots []Snapshot) error {
	seen := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.AssignmentModRevision <= 0 {
			return fmt.Errorf("assignment %q has an invalid revision", snapshot.AssignmentKey)
		}
		opaque, err := ParseFullAssignmentKey(prefix, snapshot.AssignmentKey)
		if err != nil {
			return err
		}
		if _, duplicate := seen[snapshot.AssignmentKey]; duplicate {
			return fmt.Errorf("duplicate assignment key %q", snapshot.AssignmentKey)
		}
		seen[snapshot.AssignmentKey] = struct{}{}
		record, err := Decode(snapshot.assignmentValue)
		if err != nil {
			return fmt.Errorf("assignment %q: %w", snapshot.AssignmentKey, err)
		}
		if record.AssignmentKey != opaque || record.ServerType != snapshot.ServerType ||
			record.ServerID != snapshot.OwnerID || record.OwnerEpoch != snapshot.OwnerEpoch {
			return fmt.Errorf("assignment %q identity changed in snapshot", snapshot.AssignmentKey)
		}
		registrationKey, err := FullRegistrationKey(prefix, record)
		if err != nil || registrationKey != snapshot.RegistrationKey {
			return fmt.Errorf("assignment %q has invalid registration identity", snapshot.AssignmentKey)
		}
		if snapshot.Status != StatusStale {
			return fmt.Errorf("assignment %q is %s; only stale assignments may be pruned", snapshot.AssignmentKey, snapshot.Status)
		}
		switch snapshot.StaleReason {
		case StaleReasonMissing:
			if snapshot.RegistrationVersion != 0 || snapshot.RegistrationRevision != 0 || len(snapshot.registrationValue) != 0 {
				return fmt.Errorf("assignment %q has an invalid missing-registration snapshot", snapshot.AssignmentKey)
			}
		case StaleReasonReplaced:
			if snapshot.RegistrationVersion <= 0 || snapshot.RegistrationRevision <= 0 || len(snapshot.registrationValue) == 0 ||
				snapshot.registrationCreateRevision <= 0 || uint64(snapshot.registrationCreateRevision) == snapshot.OwnerEpoch {
				return fmt.Errorf("assignment %q has an invalid replacement snapshot", snapshot.AssignmentKey)
			}
		default:
			return fmt.Errorf("assignment %q has no stale reason", snapshot.AssignmentKey)
		}
	}
	return nil
}

func SortSnapshots(snapshots []Snapshot) {
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].AssignmentKey < snapshots[j].AssignmentKey
	})
}
