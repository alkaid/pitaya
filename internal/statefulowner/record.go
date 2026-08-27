package statefulowner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/alkaid/pitaya/v2/constants"
)

const (
	RecordVersion            = 1
	AssignmentRelativePrefix = "stateful-owners/v1/"
	RegistrationPrefix       = "servers/"
)

// Record is the persisted stateful-owner assignment format. It is internal so
// runtime and maintenance tooling share one strict codec without making the
// storage format a public application API.
type Record struct {
	Version       int    `json:"version"`
	ServerType    string `json:"server_type"`
	AssignmentKey string `json:"assignment_key"`
	ServerID      string `json:"server_id"`
	OwnerEpoch    uint64 `json:"owner_epoch"`
}

func Encode(record Record) ([]byte, error) {
	return json.Marshal(record)
}

func Decode(payload []byte) (Record, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()

	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("%w: %v", constants.ErrStatefulOwnerAssignmentCorrupt, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Record{}, fmt.Errorf(
			"%w: trailing assignment data",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	if record.Version != RecordVersion || record.ServerType == "" ||
		record.AssignmentKey == "" || record.ServerID == "" || record.OwnerEpoch == 0 {
		return Record{}, fmt.Errorf(
			"%w: invalid assignment record",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	return record, nil
}

func ValidateAssignmentKey(opaque string) error {
	if len(opaque) != sha256.Size*2 {
		return fmt.Errorf("%w: invalid assignment key", constants.ErrStaleStatefulOwner)
	}
	decoded, err := hex.DecodeString(opaque)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != opaque {
		return fmt.Errorf("%w: invalid assignment key", constants.ErrStaleStatefulOwner)
	}
	return nil
}

func AssignmentRelativeKey(opaque string) (string, error) {
	if err := ValidateAssignmentKey(opaque); err != nil {
		return "", err
	}
	return AssignmentRelativePrefix + opaque, nil
}

func RegistrationRelativeKey(serverType, serverID string) (string, error) {
	if serverType == "" || serverID == "" || strings.Contains(serverType, "/") || strings.Contains(serverID, "/") {
		return "", fmt.Errorf("invalid owner registration identity")
	}
	return RegistrationPrefix + serverType + "/" + serverID, nil
}

// NamespacePrefix preserves etcd namespace semantics while rejecting prefixes
// that cannot form the canonical full keys accepted by ownerctl.
func NamespacePrefix(prefix string) (string, error) {
	if prefix != strings.TrimSpace(prefix) || strings.ContainsRune(prefix, '\x00') {
		return "", fmt.Errorf("invalid etcd namespace prefix")
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf("etcd namespace prefix must end with /")
	}
	return prefix, nil
}

func AssignmentPrefix(prefix string) (string, error) {
	prefix, err := NamespacePrefix(prefix)
	if err != nil {
		return "", err
	}
	return prefix + AssignmentRelativePrefix, nil
}

func FullAssignmentKey(prefix, opaque string) (string, error) {
	assignmentPrefix, err := AssignmentPrefix(prefix)
	if err != nil {
		return "", err
	}
	if err := ValidateAssignmentKey(opaque); err != nil {
		return "", err
	}
	return assignmentPrefix + opaque, nil
}

func ParseFullAssignmentKey(prefix, key string) (string, error) {
	assignmentPrefix, err := AssignmentPrefix(prefix)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(key, assignmentPrefix) {
		return "", fmt.Errorf("assignment key %q is outside %q", key, assignmentPrefix)
	}
	opaque := strings.TrimPrefix(key, assignmentPrefix)
	if err := ValidateAssignmentKey(opaque); err != nil {
		return "", fmt.Errorf("assignment key %q: %w", key, err)
	}
	return opaque, nil
}

func FullRegistrationKey(prefix string, record Record) (string, error) {
	prefix, err := NamespacePrefix(prefix)
	if err != nil {
		return "", err
	}
	relative, err := RegistrationRelativeKey(record.ServerType, record.ServerID)
	if err != nil {
		return "", err
	}
	return prefix + relative, nil
}
