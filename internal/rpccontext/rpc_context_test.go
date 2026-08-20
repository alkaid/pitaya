package rpccontext

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestReliableRPCJobContextLifecycle(t *testing.T) {
	_, _, ok := ReliableRPCJobFromContext(nil)
	assert.False(t, ok)
	markedFromNil := WithReliableRPCJob(nil, "job-from-nil", "digest-from-nil")
	id, digest, ok := ReliableRPCJobFromContext(markedFromNil)
	assert.True(t, ok)
	assert.Equal(t, "job-from-nil", id)
	assert.Equal(t, "digest-from-nil", digest)

	base := context.WithValue(context.Background(), struct{}{}, "preserved")
	base, cancel := context.WithDeadline(base, time.Now().Add(time.Minute))
	defer cancel()
	marked := WithReliableRPCJob(base, "job-1", "digest-1")
	id, digest, ok = ReliableRPCJobFromContext(marked)
	assert.True(t, ok)
	assert.Equal(t, "job-1", id)
	assert.Equal(t, "digest-1", digest)

	consumed := WithoutReliableRPCJob(marked)
	_, _, ok = ReliableRPCJobFromContext(consumed)
	assert.False(t, ok)
	assert.Equal(t, "preserved", consumed.Value(struct{}{}))
	deadline, ok := consumed.Deadline()
	assert.True(t, ok)
	baseDeadline, _ := base.Deadline()
	assert.Equal(t, baseDeadline, deadline)
	assert.Same(t, consumed, WithoutReliableRPCJob(consumed))
	assert.Same(t, base, WithoutReliableRPCJob(base))
	assert.Nil(t, WithoutReliableRPCJob(nil))
}

func TestRPCAdmissionAuthorityLifecycle(t *testing.T) {
	_, ok := StatefulRouteModeFromContext(nil)
	assert.False(t, ok)
	assert.False(t, RPCFanoutFromContext(nil))

	markedFromNil := WithStatefulRouteMode(nil, StatefulRouteModeAssigned)
	mode, ok := StatefulRouteModeFromContext(markedFromNil)
	assert.True(t, ok)
	assert.Equal(t, StatefulRouteModeAssigned, mode)
	assert.True(t, RPCFanoutFromContext(WithRPCFanout(nil)))

	base := context.WithValue(context.Background(), struct{}{}, "preserved")
	base, cancel := context.WithDeadline(base, time.Now().Add(time.Minute))
	defer cancel()
	marked := WithStatefulRouteMode(base, StatefulRouteModeDirect)
	marked = WithRPCFanout(marked)
	marked = WithReliableRPCJob(marked, "job-1", "digest-1")

	mode, ok = StatefulRouteModeFromContext(marked)
	assert.True(t, ok)
	assert.Equal(t, StatefulRouteModeDirect, mode)
	assert.True(t, RPCFanoutFromContext(marked))

	consumed := WithoutRPCAdmissionAuthority(marked)
	_, ok = StatefulRouteModeFromContext(consumed)
	assert.False(t, ok)
	assert.False(t, RPCFanoutFromContext(consumed))
	id, digest, reliable := ReliableRPCJobFromContext(consumed)
	assert.True(t, reliable, "reliable job authority has a separate lifecycle")
	assert.Equal(t, "job-1", id)
	assert.Equal(t, "digest-1", digest)
	assert.Equal(t, "preserved", consumed.Value(struct{}{}))
	deadline, ok := consumed.Deadline()
	assert.True(t, ok)
	baseDeadline, _ := base.Deadline()
	assert.Equal(t, baseDeadline, deadline)

	assert.Same(t, consumed, WithoutRPCAdmissionAuthority(consumed))
	assert.Same(t, base, WithoutRPCAdmissionAuthority(base))
	assert.Nil(t, WithoutRPCAdmissionAuthority(nil))
}
