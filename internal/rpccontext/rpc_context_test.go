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
