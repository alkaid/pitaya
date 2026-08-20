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

package service

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/protos"
)

type rpcStatefulOwnerInfoContextKey struct{}

type rpcAdmissionGuardContextKey struct{}

type rpcAdmissionGuard struct {
	validate func(context.Context) error

	mu        sync.Mutex
	rejection error
}

type statefulOwnerAdmission struct {
	info            RPCStatefulOwnerInfo
	resolver        cluster.StatefulOwnerResolver
	routingIdentity string
	fence           cluster.StatefulOwnerFence
}

// RPCStatefulOwnerInfo is the non-propagating write-fencing identity admitted
// for the current handler. Durable business stores should condition updates on
// AssignmentKey and a monotonically nondecreasing Epoch.
type RPCStatefulOwnerInfo struct {
	AssignmentKey string
	Epoch         uint64
	ServerID      string
	ServerType    string
}

// RPCStatefulOwnerInfoFromContext returns the current RPC's admitted stateful
// owner. The value is deliberately absent from propagation metadata so a
// downstream command must be routed and admitted independently.
func RPCStatefulOwnerInfoFromContext(ctx context.Context) (RPCStatefulOwnerInfo, bool) {
	if ctx == nil {
		return RPCStatefulOwnerInfo{}, false
	}
	info, ok := ctx.Value(rpcStatefulOwnerInfoContextKey{}).(RPCStatefulOwnerInfo)
	return info, ok
}

func withRPCStatefulOwnerInfo(ctx context.Context, info RPCStatefulOwnerInfo) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rpcStatefulOwnerInfoContextKey{}, info)
}

func withoutRPCStatefulOwnerInfo(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if _, ok := RPCStatefulOwnerInfoFromContext(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, rpcStatefulOwnerInfoContextKey{}, nil)
}

func newRPCAdmissionGuard(validate func(context.Context) error) *rpcAdmissionGuard {
	return &rpcAdmissionGuard{validate: validate}
}

func withRPCAdmissionGuard(ctx context.Context, guard *rpcAdmissionGuard) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rpcAdmissionGuardContextKey{}, guard)
}

func rpcAdmissionGuardFromContext(ctx context.Context) *rpcAdmissionGuard {
	if ctx == nil {
		return nil
	}
	guard, _ := ctx.Value(rpcAdmissionGuardContextKey{}).(*rpcAdmissionGuard)
	return guard
}

func (g *rpcAdmissionGuard) check(ctx context.Context) error {
	if g == nil || g.validate == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := g.validate(ctx)
	if err == nil {
		return nil
	}
	err = markRPCAdmissionRejected(err)
	g.mu.Lock()
	if g.rejection == nil {
		g.rejection = err
	}
	g.mu.Unlock()
	return err
}

func validateRPCAdmissionAtInvocation(authorityCtx, invocationCtx context.Context) error {
	return rpcAdmissionGuardFromContext(authorityCtx).check(invocationCtx)
}

func rpcAdmissionRejectionFromContext(ctx context.Context) error {
	guard := rpcAdmissionGuardFromContext(ctx)
	if guard == nil {
		return nil
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.rejection
}

func statefulOwnerFenceFromContext(
	ctx context.Context,
	server *cluster.Server,
) (cluster.StatefulOwnerFence, bool, error) {
	propagated := pcontext.ToMap(ctx)
	keyValue, hasKey := propagated[constants.StatefulOwnerKey]
	epochValue, hasEpoch := propagated[constants.StatefulOwnerEpochKey]
	if !hasKey && !hasEpoch {
		return cluster.StatefulOwnerFence{}, false, nil
	}
	if !hasKey || !hasEpoch {
		return cluster.StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: assignment key and epoch must be provided together",
			constants.ErrStaleStatefulOwner,
		)
	}
	assignmentKey, ok := keyValue.(string)
	if !ok || assignmentKey == "" {
		return cluster.StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: invalid assignment key",
			constants.ErrStaleStatefulOwner,
		)
	}
	epochText, ok := epochValue.(string)
	if !ok {
		return cluster.StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: assignment epoch has type %T",
			constants.ErrStaleStatefulOwner,
			epochValue,
		)
	}
	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil || epoch == 0 {
		return cluster.StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: invalid assignment epoch",
			constants.ErrStaleStatefulOwner,
		)
	}
	if server == nil || server.ID == "" {
		return cluster.StatefulOwnerFence{}, false, fmt.Errorf(
			"%w: local server identity is unavailable",
			constants.ErrStaleStatefulOwner,
		)
	}
	return cluster.StatefulOwnerFence{
		AssignmentKey: assignmentKey,
		Epoch:         epoch,
		ServerID:      server.ID,
	}, true, nil
}

func statefulRouteModeFromContext(
	ctx context.Context,
) (rpccontext.StatefulRouteMode, bool, error) {
	typedMode, hasTypedMode := rpccontext.StatefulRouteModeFromContext(ctx)
	value, hasWireMode := pcontext.ToMap(ctx)[rpccontext.StatefulRouteModeKey]
	var wireMode rpccontext.StatefulRouteMode
	if hasWireMode {
		text, ok := value.(string)
		if !ok {
			return "", false, fmt.Errorf(
				"%w: stateful route mode has type %T",
				constants.ErrStaleStatefulOwner,
				value,
			)
		}
		wireMode = rpccontext.StatefulRouteMode(text)
		if wireMode != rpccontext.StatefulRouteModeAssigned &&
			wireMode != rpccontext.StatefulRouteModeDirect &&
			wireMode != rpccontext.StatefulRouteModeFanout {
			return "", false, fmt.Errorf(
				"%w: invalid stateful route mode %q",
				constants.ErrStaleStatefulOwner,
				text,
			)
		}
	}
	if hasTypedMode && hasWireMode && typedMode != wireMode {
		return "", false, fmt.Errorf(
			"%w: stateful route mode authorities disagree",
			constants.ErrStaleStatefulOwner,
		)
	}
	if hasTypedMode {
		return typedMode, true, nil
	}
	return wireMode, hasWireMode, nil
}

func statefulRoutingKeyFromRequest(req *protos.Request) (string, error) {
	if req == nil || req.Session == nil {
		return "", constants.ErrStatefulRouteKeyNotFound
	}
	return cluster.StatefulOwnerRoutingKey(
		req.Session.GetUid(),
		req.FrontendID,
		req.Session.GetId(),
	)
}

func (a statefulOwnerAdmission) admitted() bool {
	return a.resolver != nil
}

func (a statefulOwnerAdmission) revalidate(ctx context.Context, serverType string) error {
	if !a.admitted() {
		return nil
	}
	return a.resolver.ValidateStatefulOwner(ctx, serverType, a.routingIdentity, a.fence)
}

func (r *RemoteService) admitStatefulOwner(
	ctx context.Context,
	req *protos.Request,
) (statefulOwnerAdmission, error) {
	fence, hasFence, err := statefulOwnerFenceFromContext(ctx, r.server)
	if err != nil {
		return statefulOwnerAdmission{}, err
	}
	resolver, supportsFencing := r.serviceDiscovery.(cluster.StatefulOwnerResolver)
	fanout, err := fanoutIdempotencyScope(ctx)
	if err != nil {
		return statefulOwnerAdmission{}, err
	}
	mode, hasMode, err := statefulRouteModeFromContext(ctx)
	if err != nil {
		return statefulOwnerAdmission{}, err
	}
	if rpccontext.RPCFanoutFromContext(ctx) && !hasMode {
		mode, hasMode = rpccontext.StatefulRouteModeFanout, true
	}
	if fanout != (hasMode && mode == rpccontext.StatefulRouteModeFanout) {
		return statefulOwnerAdmission{}, fmt.Errorf(
			"%w: fanout marker and stateful route mode must agree",
			constants.ErrStaleStatefulOwner,
		)
	}

	requiresMode := supportsFencing && r.server != nil && !r.server.Frontend &&
		req != nil && req.Session != nil
	if !hasMode {
		if requiresMode || hasFence {
			return statefulOwnerAdmission{}, fmt.Errorf(
				"%w: framework stateful route mode is required",
				constants.ErrStaleStatefulOwner,
			)
		}
		return statefulOwnerAdmission{}, nil
	}

	switch mode {
	case rpccontext.StatefulRouteModeDirect, rpccontext.StatefulRouteModeFanout:
		if hasFence {
			return statefulOwnerAdmission{}, fmt.Errorf(
				"%w: %s routing must not carry assignment metadata",
				constants.ErrStaleStatefulOwner,
				mode,
			)
		}
		return statefulOwnerAdmission{}, nil
	case rpccontext.StatefulRouteModeAssigned:
		if !hasFence {
			return statefulOwnerAdmission{}, fmt.Errorf(
				"%w: stateful owner assignment is required",
				constants.ErrStaleStatefulOwner,
			)
		}
	default:
		return statefulOwnerAdmission{}, fmt.Errorf(
			"%w: invalid stateful route mode",
			constants.ErrStaleStatefulOwner,
		)
	}

	if !supportsFencing {
		return statefulOwnerAdmission{}, fmt.Errorf(
			"%w: service discovery cannot validate assignment metadata",
			constants.ErrStatefulOwnerAssignmentUnavailable,
		)
	}
	routingIdentity, err := statefulRoutingKeyFromRequest(req)
	if err != nil {
		return statefulOwnerAdmission{}, fmt.Errorf("%w: %v", constants.ErrStaleStatefulOwner, err)
	}
	if err := resolver.ValidateStatefulOwner(ctx, r.server.Type, routingIdentity, fence); err != nil {
		return statefulOwnerAdmission{}, err
	}
	return statefulOwnerAdmission{
		info: RPCStatefulOwnerInfo{
			AssignmentKey: fence.AssignmentKey,
			Epoch:         fence.Epoch,
			ServerID:      fence.ServerID,
			ServerType:    r.server.Type,
		},
		resolver:        resolver,
		routingIdentity: routingIdentity,
		fence:           fence,
	}, nil
}
