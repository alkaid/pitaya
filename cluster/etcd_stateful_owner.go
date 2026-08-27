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
	"fmt"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/internal/statefulowner"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	statefulOwnerRecordVersion  = statefulowner.RecordVersion
	statefulOwnerResolveRetries = 16
	statefulOwnerKeyPrefix      = statefulowner.AssignmentRelativePrefix
)

type statefulOwnerRecord = statefulowner.Record

var _ StatefulOwnerResolver = (*etcdServiceDiscovery)(nil)
var _ StatefulOwnerHandoff = (*etcdServiceDiscovery)(nil)

func statefulOwnerAssignmentKey(serverType, routingIdentity string) (string, string, error) {
	opaque, err := StatefulOwnerAssignmentKey(serverType, routingIdentity)
	if err != nil {
		return "", "", err
	}
	return opaque, statefulOwnerKeyPrefix + opaque, nil
}

func statefulOwnerStorageKey(opaque string) (string, error) {
	return statefulowner.AssignmentRelativeKey(opaque)
}

func encodeStatefulOwnerRecord(record statefulOwnerRecord) ([]byte, error) {
	return statefulowner.Encode(record)
}

func decodeStatefulOwnerRecord(payload []byte) (statefulOwnerRecord, error) {
	return statefulowner.Decode(payload)
}

// ResolveStatefulOwner preserves an existing live assignment even when the
// caller's consistent-hash candidate changes. An assignment moves only after
// the registration generation it names disappears or is replaced. The etcd
// ModRevision of the successful CAS is the handoff epoch.
func (sd *etcdServiceDiscovery) ResolveStatefulOwner(
	ctx context.Context,
	serverType string,
	routingIdentity string,
	candidateServerID string,
) (*Server, StatefulOwnerFence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sd == nil || sd.cli == nil {
		return nil, StatefulOwnerFence{}, constants.ErrStatefulOwnerAssignmentUnavailable
	}
	if candidateServerID == "" {
		return nil, StatefulOwnerFence{}, fmt.Errorf(
			"%w: candidate server ID is required",
			constants.ErrStatefulOwnerAssignmentUnavailable,
		)
	}
	opaque, assignmentKey, err := statefulOwnerAssignmentKey(serverType, routingIdentity)
	if err != nil {
		return nil, StatefulOwnerFence{}, err
	}

	for range statefulOwnerResolveRetries {
		assignmentResponse, getErr := sd.cli.Get(ctx, assignmentKey)
		if getErr != nil {
			return nil, StatefulOwnerFence{}, fmt.Errorf(
				"%w: read assignment: %v",
				constants.ErrStatefulOwnerAssignmentUnavailable,
				getErr,
			)
		}
		var assignment *mvccpb.KeyValue
		if len(assignmentResponse.Kvs) > 0 {
			assignment = assignmentResponse.Kvs[0]
			owner, fence, current, readErr := sd.readStatefulOwner(
				ctx,
				opaque,
				serverType,
				assignment,
			)
			if readErr != nil {
				return nil, StatefulOwnerFence{}, readErr
			}
			if current {
				return owner, fence, nil
			}
		}

		candidate, candidateEpoch, candidateErr := sd.readRegisteredStatefulOwner(
			ctx,
			serverType,
			candidateServerID,
		)
		if candidateErr != nil {
			return nil, StatefulOwnerFence{}, candidateErr
		}
		record := statefulOwnerRecord{
			Version:       statefulOwnerRecordVersion,
			ServerType:    serverType,
			AssignmentKey: opaque,
			ServerID:      candidateServerID,
			OwnerEpoch:    candidateEpoch,
		}
		payload, encodeErr := encodeStatefulOwnerRecord(record)
		if encodeErr != nil {
			return nil, StatefulOwnerFence{}, encodeErr
		}
		expectedRevision := int64(0)
		if assignment != nil {
			expectedRevision = assignment.ModRevision
		}
		transaction, commitErr := sd.cli.Txn(ctx).
			If(
				clientv3.Compare(clientv3.ModRevision(assignmentKey), "=", expectedRevision),
				clientv3.Compare(clientv3.CreateRevision(getKey(candidateServerID, serverType)), "=", int64(candidateEpoch)),
			).
			Then(clientv3.OpPut(assignmentKey, string(payload))).
			Commit()
		if commitErr != nil {
			return nil, StatefulOwnerFence{}, fmt.Errorf(
				"%w: claim assignment: %v",
				constants.ErrStatefulOwnerAssignmentUnavailable,
				commitErr,
			)
		}
		if !transaction.Succeeded {
			continue
		}
		fence := StatefulOwnerFence{
			AssignmentKey: opaque,
			Epoch:         uint64(transaction.Header.Revision),
			ServerID:      candidateServerID,
		}
		return candidate.WithStatefulOwnerFence(fence), fence, nil
	}
	return nil, StatefulOwnerFence{}, fmt.Errorf(
		"%w: assignment changed too frequently",
		constants.ErrStatefulOwnerAssignmentUnavailable,
	)
}

func (sd *etcdServiceDiscovery) readStatefulOwner(
	ctx context.Context,
	opaque string,
	serverType string,
	assignment *mvccpb.KeyValue,
) (*Server, StatefulOwnerFence, bool, error) {
	record, err := decodeStatefulOwnerRecord(assignment.Value)
	if err != nil {
		return nil, StatefulOwnerFence{}, false, err
	}
	if record.ServerType != serverType || record.AssignmentKey != opaque ||
		string(assignment.Key) != statefulOwnerKeyPrefix+opaque {
		return nil, StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: assignment identity does not match its key",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	check, err := sd.cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(string(assignment.Key)), "=", assignment.ModRevision),
			clientv3.Compare(clientv3.CreateRevision(getKey(record.ServerID, record.ServerType)), "=", int64(record.OwnerEpoch)),
		).
		Then(
			clientv3.OpGet(string(assignment.Key)),
			clientv3.OpGet(getKey(record.ServerID, record.ServerType)),
		).
		Commit()
	if err != nil {
		return nil, StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: validate assignment: %v",
			constants.ErrStatefulOwnerAssignmentUnavailable,
			err,
		)
	}
	if !check.Succeeded {
		return nil, StatefulOwnerFence{}, false, nil
	}
	if len(check.Responses) != 2 || check.Responses[1].GetResponseRange() == nil ||
		len(check.Responses[1].GetResponseRange().Kvs) != 1 {
		return nil, StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: owner validation returned an invalid response",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	serverKV := check.Responses[1].GetResponseRange().Kvs[0]
	server, err := parseServerForKey(serverKV.Value, record.ServerType, record.ServerID)
	if err != nil {
		return nil, StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: %v",
			constants.ErrStatefulOwnerAssignmentCorrupt,
			err,
		)
	}
	server.SetOwnerEpoch(record.OwnerEpoch)
	fence := StatefulOwnerFence{
		AssignmentKey: opaque,
		Epoch:         uint64(assignment.ModRevision),
		ServerID:      record.ServerID,
	}
	return server.WithStatefulOwnerFence(fence), fence, true, nil
}

func (sd *etcdServiceDiscovery) readRegisteredStatefulOwner(
	ctx context.Context,
	serverType string,
	serverID string,
) (*Server, uint64, error) {
	response, err := sd.cli.Get(ctx, getKey(serverID, serverType))
	if err != nil {
		return nil, 0, fmt.Errorf(
			"%w: read owner registration: %v",
			constants.ErrStatefulOwnerAssignmentUnavailable,
			err,
		)
	}
	if len(response.Kvs) != 1 || response.Kvs[0].CreateRevision <= 0 {
		return nil, 0, fmt.Errorf(
			"%w: owner registration %s/%s is absent",
			constants.ErrStaleStatefulOwner,
			serverType,
			serverID,
		)
	}
	server, err := parseServerForKey(response.Kvs[0].Value, serverType, serverID)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", constants.ErrStatefulOwnerAssignmentCorrupt, err)
	}
	epoch := uint64(response.Kvs[0].CreateRevision)
	server.SetOwnerEpoch(epoch)
	return server, epoch, nil
}

// ValidateStatefulOwner performs a linearizable admission check against both
// the per-key assignment and this process's current registration generation.
func (sd *etcdServiceDiscovery) ValidateStatefulOwner(
	ctx context.Context,
	serverType string,
	routingIdentity string,
	fence StatefulOwnerFence,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if sd == nil || sd.cli == nil || sd.server == nil {
		return constants.ErrStatefulOwnerAssignmentUnavailable
	}
	expectedOpaque, assignmentKey, err := statefulOwnerAssignmentKey(serverType, routingIdentity)
	if err != nil {
		return err
	}
	if fence.AssignmentKey != expectedOpaque || fence.Epoch == 0 ||
		fence.ServerID != sd.server.ID || serverType != sd.server.Type {
		return constants.ErrStaleStatefulOwner
	}
	localEpoch, valid := sd.server.OwnerEpochSnapshot()
	if !valid || localEpoch == 0 {
		return constants.ErrStaleStatefulOwner
	}
	check, err := sd.cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(assignmentKey), "=", int64(fence.Epoch)),
			clientv3.Compare(clientv3.CreateRevision(getKey(sd.server.ID, serverType)), "=", int64(localEpoch)),
		).
		Then(clientv3.OpGet(assignmentKey)).
		Commit()
	if err != nil {
		return fmt.Errorf(
			"%w: validate assignment: %v",
			constants.ErrStatefulOwnerAssignmentUnavailable,
			err,
		)
	}
	if !check.Succeeded {
		return constants.ErrStaleStatefulOwner
	}
	if len(check.Responses) != 1 || check.Responses[0].GetResponseRange() == nil ||
		len(check.Responses[0].GetResponseRange().Kvs) != 1 {
		return fmt.Errorf(
			"%w: assignment validation returned an invalid response",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	record := check.Responses[0].GetResponseRange().Kvs[0]
	decoded, err := decodeStatefulOwnerRecord(record.Value)
	if err != nil {
		return err
	}
	if string(record.Key) != assignmentKey || uint64(record.ModRevision) != fence.Epoch ||
		decoded.ServerType != serverType || decoded.AssignmentKey != expectedOpaque ||
		decoded.ServerID != fence.ServerID || decoded.OwnerEpoch != localEpoch {
		return fmt.Errorf(
			"%w: assignment identity does not match its key",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	return nil
}

// HandoffStatefulOwner atomically activates a live successor for an existing
// assignment. The caller owns the drain protocol before this CAS; admission and
// business-store fencing reject the old epoch after it commits.
func (sd *etcdServiceDiscovery) HandoffStatefulOwner(
	ctx context.Context,
	serverType string,
	current StatefulOwnerFence,
	successorServerID string,
) (*Server, StatefulOwnerFence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sd == nil || sd.cli == nil {
		return nil, StatefulOwnerFence{}, constants.ErrStatefulOwnerAssignmentUnavailable
	}
	assignmentKey, err := statefulOwnerStorageKey(current.AssignmentKey)
	if err != nil {
		return nil, StatefulOwnerFence{}, err
	}
	if current.Epoch == 0 || current.ServerID == "" || successorServerID == "" ||
		successorServerID == current.ServerID {
		return nil, StatefulOwnerFence{}, constants.ErrStaleStatefulOwner
	}
	response, err := sd.cli.Get(ctx, assignmentKey)
	if err != nil {
		return nil, StatefulOwnerFence{}, fmt.Errorf(
			"%w: read assignment for handoff: %v",
			constants.ErrStatefulOwnerAssignmentUnavailable,
			err,
		)
	}
	if len(response.Kvs) != 1 || uint64(response.Kvs[0].ModRevision) != current.Epoch {
		return nil, StatefulOwnerFence{}, constants.ErrStaleStatefulOwner
	}
	record, err := decodeStatefulOwnerRecord(response.Kvs[0].Value)
	if err != nil {
		return nil, StatefulOwnerFence{}, err
	}
	if record.ServerType != serverType || record.ServerID != current.ServerID ||
		record.AssignmentKey != current.AssignmentKey || string(response.Kvs[0].Key) != assignmentKey {
		return nil, StatefulOwnerFence{}, fmt.Errorf(
			"%w: assignment identity does not match its key",
			constants.ErrStatefulOwnerAssignmentCorrupt,
		)
	}
	successor, successorEpoch, err := sd.readRegisteredStatefulOwner(ctx, serverType, successorServerID)
	if err != nil {
		return nil, StatefulOwnerFence{}, err
	}
	nextRecord := statefulOwnerRecord{
		Version:       statefulOwnerRecordVersion,
		ServerType:    serverType,
		AssignmentKey: current.AssignmentKey,
		ServerID:      successorServerID,
		OwnerEpoch:    successorEpoch,
	}
	payload, err := encodeStatefulOwnerRecord(nextRecord)
	if err != nil {
		return nil, StatefulOwnerFence{}, err
	}
	transaction, err := sd.cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(assignmentKey), "=", int64(current.Epoch)),
			clientv3.Compare(
				clientv3.CreateRevision(getKey(current.ServerID, serverType)),
				"=",
				int64(record.OwnerEpoch),
			),
			clientv3.Compare(
				clientv3.CreateRevision(getKey(successorServerID, serverType)),
				"=",
				int64(successorEpoch),
			),
		).
		Then(clientv3.OpPut(assignmentKey, string(payload))).
		Commit()
	if err != nil {
		return nil, StatefulOwnerFence{}, fmt.Errorf(
			"%w: handoff assignment: %v",
			constants.ErrStatefulOwnerAssignmentUnavailable,
			err,
		)
	}
	if !transaction.Succeeded {
		return nil, StatefulOwnerFence{}, constants.ErrStaleStatefulOwner
	}
	next := StatefulOwnerFence{
		AssignmentKey: current.AssignmentKey,
		Epoch:         uint64(transaction.Header.Revision),
		ServerID:      successorServerID,
	}
	return successor.WithStatefulOwnerFence(next), next, nil
}
