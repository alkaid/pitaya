package co

import (
	"context"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
)

func newBenchmarkStatefulPool(b *testing.B) *StatefulPool {
	b.Helper()

	pool, err := NewStatefulPool(config.GoPool{
		Name:                b.Name(),
		Expire:              time.Hour,
		RunningTaskTimeout:  time.Hour,
		DisablePurge:        true,
		DisablePurgeRunning: true,
		DisableTimeoutWatch: true,
	}, nil)
	if err != nil {
		b.Fatalf("create stateful pool: %v", err)
	}
	b.Cleanup(pool.pool.Release)
	return pool
}

func BenchmarkStatefulPoolGoSameKey(b *testing.B) {
	pool := newBenchmarkStatefulPool(b)
	ctx := context.Background()
	task := func(context.Context) {}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done, err := pool.Go(ctx, 1, task, true)
		if err != nil {
			b.Fatal(err)
		}
		<-done
	}
}

func BenchmarkStatefulPoolWaitSameKey(b *testing.B) {
	pool := newBenchmarkStatefulPool(b)
	ctx := context.Background()
	task := func(context.Context) {}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := pool.Wait(ctx, 1, task, true); err != nil {
			b.Fatal(err)
		}
	}
}
