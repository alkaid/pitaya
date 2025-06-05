package co

import (
	"context"
	"math/rand/v2"
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
		TimeoutBuckets:      []time.Duration{time.Second * 5, time.Second * 10},
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

// Test_HugeAmount 测试大量协程情况下能否正常跑完
//
//	@param t
func Test_HugeAmount(t *testing.T) {
	p := newPool()
	var wg sync.WaitGroup
	ctx := context.Background()
	var GoByID = func(goID int, task func()) {
		p.Go(ctx, goID, func(ctx context.Context) {
			task()
		}, false)
	}
	var calc = func() {
		// 模拟一个耗时操作
		time.Sleep(time.Microsecond * time.Duration(rand.IntN(1000)))
		wg.Done()
	}
	for k := 0; k < 1000; k++ {
		wg.Add(1)
		GoByID(k, func() {
			wg.Add(1)
			GoByID(k+1, func() {
				for i := 0; i < 5; i++ {
					wg.Add(1)
					GoByID(i, calc)
				}
				calc()
			})
			wg.Add(1)
			GoByID(k+2, func() {
				for i := 0; i < 5; i++ {
					wg.Add(1)
					GoByID(i, calc)
				}
				calc()
			})
			wg.Add(1)
			GoByID(k+3, func() {
				for i := 0; i < 5; i++ {
					wg.Add(1)
					GoByID(i, calc)
				}
				calc()
			})
			calc()
		})
	}
	wg.Wait()
	time.Sleep(time.Second * 10)
}
