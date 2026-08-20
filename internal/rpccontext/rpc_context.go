package rpccontext

import "context"

const (
	// ReliableJobKey identifies the Worker JID on the wire. Outbound request
	// construction replaces any propagated value with the private context value.
	ReliableJobKey = "rpc.reliable-job"
	// ReliableJobMetadataDigestKey binds the original routing metadata to the
	// logical command without disclosing that metadata to the target handler.
	ReliableJobMetadataDigestKey = "rpc.reliable-metadata-sha256"
)

type reliableRPCJobContextKey struct{}

type reliableRPCJobContext struct {
	id             string
	metadataDigest string
}

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
