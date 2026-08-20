package rpccontext

import "context"

const (
	// ReliableJobKey identifies the Worker JID on the wire. Outbound request
	// construction replaces any propagated value with the private context value.
	ReliableJobKey = "rpc.reliable-job"
	// ReliableJobMetadataDigestKey binds the original routing metadata to the
	// logical command without disclosing that metadata to the target handler.
	ReliableJobMetadataDigestKey = "rpc.reliable-metadata-sha256"
	// StatefulRouteModeKey identifies the framework-selected routing contract
	// for one RPC hop. Outbound preparation removes propagated values and emits
	// this field only from the process-local marker below.
	StatefulRouteModeKey = "rpc.stateful-route-mode"
)

// StatefulRouteMode describes how a session-bearing backend target was
// selected. It is intentionally defined in an internal package so application
// code cannot manufacture the process-local authority marker.
type StatefulRouteMode string

const (
	StatefulRouteModeAssigned StatefulRouteMode = "assigned"
	StatefulRouteModeDirect   StatefulRouteMode = "direct"
	StatefulRouteModeFanout   StatefulRouteMode = "fanout"
)

type reliableRPCJobContextKey struct{}

type reliableRPCJobContext struct {
	id             string
	metadataDigest string
}

type statefulRouteModeContextKey struct{}
type rpcFanoutContextKey struct{}

// WithReliableRPCJob marks a Worker-owned RPC context. This package is internal
// so applications cannot manufacture the typed marker through Pitaya's API.
func WithReliableRPCJob(ctx context.Context, id, metadataDigest string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, reliableRPCJobContextKey{}, reliableRPCJobContext{
		id:             id,
		metadataDigest: metadataDigest,
	})
}

// ReliableRPCJobFromContext returns the private Worker marker, if present.
func ReliableRPCJobFromContext(ctx context.Context) (id, metadataDigest string, ok bool) {
	if ctx == nil {
		return "", "", false
	}
	job, ok := ctx.Value(reliableRPCJobContextKey{}).(reliableRPCJobContext)
	if !ok {
		return "", "", false
	}
	return job.id, job.metadataDigest, true
}

// WithoutReliableRPCJob ends the process-local authority of a Worker marker.
// The wire representation is consumed separately at RPC admission.
func WithoutReliableRPCJob(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if _, _, ok := ReliableRPCJobFromContext(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, reliableRPCJobContextKey{}, nil)
}

// WithStatefulRouteMode records the framework-selected route mode for the next
// RPC hop. Fanout callers must use WithRPCFanout instead.
func WithStatefulRouteMode(ctx context.Context, mode StatefulRouteMode) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, statefulRouteModeContextKey{}, mode)
}

// StatefulRouteModeFromContext returns the private route-mode marker.
func StatefulRouteModeFromContext(ctx context.Context) (StatefulRouteMode, bool) {
	if ctx == nil {
		return "", false
	}
	mode, ok := ctx.Value(statefulRouteModeContextKey{}).(StatefulRouteMode)
	return mode, ok
}

// WithRPCFanout marks a framework-owned multi-target RPC. The marker controls
// both per-target idempotency scope and stateful admission bypass.
func WithRPCFanout(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rpcFanoutContextKey{}, true)
}

// RPCFanoutFromContext reports whether the current process authorized fanout.
func RPCFanoutFromContext(ctx context.Context) bool {
	return ctx != nil && ctx.Value(rpcFanoutContextKey{}) == true
}

// WithoutRPCAdmissionAuthority ends process-local route and fanout authority at
// the current RPC admission boundary.
func WithoutRPCAdmissionAuthority(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if _, hasMode := StatefulRouteModeFromContext(ctx); hasMode {
		ctx = context.WithValue(ctx, statefulRouteModeContextKey{}, nil)
	}
	if RPCFanoutFromContext(ctx) {
		ctx = context.WithValue(ctx, rpcFanoutContextKey{}, nil)
	}
	return ctx
}
