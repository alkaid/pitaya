package co

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func help() {
	NewHolder(CoroutineConfig{
		Nums:    1000,
		Buffers: 1000,
	}).start()
}

func TestAwait(t *testing.T) {
	help()
	d, _ := Await[string](context.Background(), func(ctx context.Context, co Coroutine) (any, error) {
		return "test", nil
	})
	assert.Equal(t, "test", d)
}
