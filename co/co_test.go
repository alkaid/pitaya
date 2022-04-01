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
	ctx := context.Background()
	d, _ := LooperInstance.Async(ctx, func(ctx context.Context, co Coroutine) (any, error) {
		return "test", nil
	}).Wait(ctx)
	assert.Equal(t, "test", d)
	// a, _ := Await(ctx, func(ctx context.Context) (string, error) {
	// 	return "ttt", nil
	// })
	// t.Log("a=", a)
}
