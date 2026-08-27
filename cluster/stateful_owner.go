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
	"strconv"

	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/session"
)

// StatefulOwnerFence identifies one durable assignment of a routing key to a
// server identity. AssignmentKey is an opaque digest and Epoch is the etcd
// modification revision which activated the assignment.
type StatefulOwnerFence struct {
	AssignmentKey string
	Epoch         uint64
	ServerID      string
}

// StatefulOwnerResolver is an optional service-discovery capability. Routers
// use it to preserve one owner across topology changes; RPC admission uses it
// to reject requests issued against a superseded assignment.
type StatefulOwnerResolver interface {
	ResolveStatefulOwner(
		ctx context.Context,
		serverType string,
		routingIdentity string,
		candidateServerID string,
	) (*Server, StatefulOwnerFence, error)
	ValidateStatefulOwner(
		ctx context.Context,
		serverType string,
		routingIdentity string,
		fence StatefulOwnerFence,
	) error
}

// StatefulOwnerAssignmentKey returns the protocol-defined exact identity for
// one server type and canonical routing identity. Resolver implementations must
// use this key in StatefulOwnerFence so pre-fenced targets and receiving peers
// can bind a token to the current request without retaining its raw identity.
func StatefulOwnerAssignmentKey(serverType, routingIdentity string) (string, error) {
	if serverType == "" || routingIdentity == "" {
		return "", fmt.Errorf(
			"%w: server type and routing key are required",
			constants.ErrStatefulOwnerAssignmentUnavailable,
		)
	}
	hasher := sha256.New()
	_, _ = fmt.Fprintf(
		hasher,
		"%d:%s%d:%s",
		len(serverType),
		serverType,
		len(routingIdentity),
		routingIdentity,
	)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// StatefulOwnerHandoff is the optional administrative side of durable owner
// assignments. Callers must drain the current owner before activating the
// successor and must fence business writes with the returned epoch.
type StatefulOwnerHandoff interface {
	HandoffStatefulOwner(
		ctx context.Context,
		serverType string,
		current StatefulOwnerFence,
		successorServerID string,
	) (*Server, StatefulOwnerFence, error)
}

// StatefulRoutingKey returns the stable key used by the default stateful
// consistent-hash router. It preserves the pre-fencing hash input format;
// assignment fencing uses the domain-separated StatefulOwnerRoutingKey.
func StatefulRoutingKey(sess session.Session) (string, error) {
	if sess == nil {
		return "", constants.ErrStatefulRouteKeyNotFound
	}
	if uid := sess.UID(); uid != "" {
		return uid, nil
	}
	frontendID := sess.GetFrontendID()
	frontendSessionID := sess.GetFrontendSessionID()
	if frontendID == "" || frontendSessionID <= 0 {
		return "", constants.ErrStatefulRouteKeyNotFound
	}
	return fmt.Sprintf("%s-%d", frontendID, frontendSessionID), nil
}

// StatefulOwnerRoutingKey returns the canonical identity used by assignment
// fencing. UID and frontend/session identities use separate, length-framed
// domains so values which share the legacy consistent-hash text cannot share a
// durable assignment.
func StatefulOwnerRoutingKey(uid, frontendID string, frontendSessionID int64) (string, error) {
	if uid != "" {
		return fmt.Sprintf("v1:uid:%d:%s", len(uid), uid), nil
	}
	if frontendID == "" || frontendSessionID <= 0 {
		return "", constants.ErrStatefulRouteKeyNotFound
	}
	return fmt.Sprintf("v1:session:%d:%s:%d", len(frontendID), frontendID, frontendSessionID), nil
}

// ResolveStatefulServer attaches a durable assignment to a request-scoped
// server snapshot when discovery supports fencing. Discoveries without the
// optional capability retain their legacy routing behavior.
func ResolveStatefulServer(
	ctx context.Context,
	discovery ServiceDiscovery,
	serverType string,
	target *Server,
	sess session.Session,
) (*Server, error) {
	if target == nil || target.Frontend || sess == nil || rpccontext.RPCFanoutFromContext(ctx) {
		return target, nil
	}
	resolver, ok := discovery.(StatefulOwnerResolver)
	if !ok {
		return target, nil
	}
	if target.Type != serverType {
		return nil, fmt.Errorf(
			"%w: candidate type %q does not match route type %q",
			constants.ErrStatefulOwnerAssignmentCorrupt,
			target.Type,
			serverType,
		)
	}
	routingIdentity, err := StatefulOwnerRoutingKey(
		sess.UID(),
		sess.GetFrontendID(),
		sess.GetFrontendSessionID(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: server type %s", err, serverType)
	}
	if fence, ok := target.StatefulOwnerFenceSnapshot(); ok {
		if fence.ServerID != target.ID || fence.AssignmentKey == "" || fence.Epoch == 0 {
			return nil, constants.ErrStatefulOwnerAssignmentCorrupt
		}
		expectedKey, keyErr := StatefulOwnerAssignmentKey(serverType, routingIdentity)
		if keyErr != nil {
			return nil, keyErr
		}
		if fence.AssignmentKey != expectedKey {
			return nil, constants.ErrStaleStatefulOwner
		}
		return target, nil
	}
	owner, fence, err := resolver.ResolveStatefulOwner(ctx, serverType, routingIdentity, target.ID)
	if err != nil {
		return nil, err
	}
	expectedKey, keyErr := StatefulOwnerAssignmentKey(serverType, routingIdentity)
	if keyErr != nil {
		return nil, keyErr
	}
	if owner == nil || owner.ID != fence.ServerID || owner.Type != serverType ||
		fence.AssignmentKey != expectedKey || fence.Epoch == 0 {
		return nil, constants.ErrStatefulOwnerAssignmentCorrupt
	}
	return owner.WithStatefulOwnerFence(fence), nil
}

// WithStatefulOwnerFence returns a request-scoped server snapshot carrying a
// per-key assignment. The service-discovery cache entry is never mutated.
func (s *Server) WithStatefulOwnerFence(fence StatefulOwnerFence) *Server {
	if s == nil {
		return nil
	}
	clone := *s
	clone.statefulOwnerFence = &StatefulOwnerFence{
		AssignmentKey: fence.AssignmentKey,
		Epoch:         fence.Epoch,
		ServerID:      fence.ServerID,
	}
	return &clone
}

// StatefulOwnerFenceSnapshot returns the request-scoped assignment, if any.
func (s *Server) StatefulOwnerFenceSnapshot() (StatefulOwnerFence, bool) {
	if s == nil || s.statefulOwnerFence == nil {
		return StatefulOwnerFence{}, false
	}
	return *s.statefulOwnerFence, true
}

func (s *Server) withStatefulOwnerFencingMetadata(ctx context.Context) context.Context {
	propagation := pcontext.ToMap(ctx)
	_, hadKey := propagation[constants.StatefulOwnerKey]
	_, hadEpoch := propagation[constants.StatefulOwnerEpochKey]
	fence, hasFence := s.StatefulOwnerFenceSnapshot()
	if !hasFence && !hadKey && !hadEpoch {
		return ctx
	}

	delete(propagation, constants.StatefulOwnerKey)
	delete(propagation, constants.StatefulOwnerEpochKey)
	if hasFence {
		propagation[constants.StatefulOwnerKey] = fence.AssignmentKey
		propagation[constants.StatefulOwnerEpochKey] = strconv.FormatUint(fence.Epoch, 10)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, constants.PropagateCtxKey, propagation)
}
