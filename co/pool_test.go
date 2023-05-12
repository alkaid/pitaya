package co

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/topfreegames/pitaya/v2/config"
)

func newPool() *StatefulPool {
	p, _ := NewStatefulPool(config.GoPool{
		Name:                "test",
		Expire:              time.Minute * 10,
		TaskBuffer:          100,
		DisableTimeoutWatch: false,
		TimeoutBuckets:      nil,
	}, nil)
	return p
}

func TestStatefulPool_Go(t *testing.T) {
	p := newPool()
	var wg sync.WaitGroup
	for i := 1; i < 100000; i++ {
		wg.Add(1)
		p.Go(context.Background(), i, func(ctx context.Context) {
			// time.Sleep(10 * time.Second)
			wg.Done()
		}, false)
	}
	wg.Wait()
	time.Sleep(time.Second * 5)
}
