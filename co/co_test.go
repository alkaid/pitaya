package co

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/topfreegames/pitaya/v2/config"
)

func help() {
	NewHolder(&config.CoroutineConfig{
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
