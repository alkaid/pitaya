package co

import (
	"context"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
)

const benchmarkStatefulPoolReleaseTimeout = 30 * time.Second

func cleanupBenchmarkStatefulPool(b *testing.B, pool *StatefulPool) {
	b.Helper()
	b.Cleanup(func() {
		if err := pool.pool.ReleaseTimeout(benchmarkStatefulPoolReleaseTimeout); err != nil {
			b.Errorf("release stateful pool: %v", err)
		}
	})
}

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
	cleanupBenchmarkStatefulPool(b, pool)
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

func BenchmarkStatefulPoolGoUniqueKeys(b *testing.B) {
	pool, err := NewStatefulPool(config.GoPool{
		Name:                b.Name(),
		DisableTimeoutWatch: true,
	}, nil)
	if err != nil {
		b.Fatalf("create stateful pool: %v", err)
	}
	cleanupBenchmarkStatefulPool(b, pool)
	ctx := context.Background()
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	task := func(context.Context) { <-release }
	done := make([]<-chan struct{}, b.N)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done[i], err = pool.Go(ctx, i+1, task, true)
		if err != nil {
			b.Fatal(err)
		}
	}
	close(release)
	released = true
	for _, taskDone := range done {
		<-taskDone
	}
}
