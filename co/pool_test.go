package co

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/topfreegames/pitaya/v2/config"
)

func newPool(name string) *StatefulPool {
	p, _ := NewStatefulPool(config.GoPool{
		Name:                name,
		Expire:              time.Minute * 10,
		TaskBuffer:          100,
		DisableTimeoutWatch: false,
		TimeoutBuckets:      []time.Duration{time.Second * 5, time.Second * 6},
	}, nil)
	return p
}

func TestStatefulPool_Go(t *testing.T) {
	p := newPool("test")
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
	p := newPool("test")
	var wg sync.WaitGroup
	ctx := context.Background()
	var GoByID = func(goID int, task func()) {
		wg.Add(1)
		p.Go(ctx, goID, func(ctx context.Context) {
			task()
			wg.Done()
		}, false)
	}
	var calc = func() {
		// 模拟一个耗时操作
		time.Sleep(time.Microsecond * time.Duration(rand.IntN(1000)))
	}
	for k := 0; k < 1000; k++ {
		GoByID(k, func() {
			GoByID(k+1, func() {
				for i := 0; i < 5; i++ {
					GoByID(i, calc)
				}
				calc()
			})
			GoByID(k+2, func() {
				for i := 0; i < 5; i++ {
					GoByID(i, calc)
				}
				calc()
			})
			GoByID(k+3, func() {
				for i := 0; i < 5; i++ {
					GoByID(i, calc)
				}
				calc()
			})
			calc()
		})
	}
	GoByID(10000, func() {
		time.Sleep(time.Second * 15)
	})
	wg.Wait()
}

// Test_MultiPool 测试多个线程池情况下能否正常跑完
//
//	@param t
func Test_MultiPool(t *testing.T) {
	p := newPool("user")
	rp := newPool("room")
	var wg sync.WaitGroup
	ctx := context.Background()
	var GoByID = func(goID int, task func()) {
		wg.Add(1)
		p.Go(ctx, goID, func(ctx context.Context) {
			task()
			wg.Done()
		}, false)
	}
	var calc = func() {
		// 模拟一个耗时操作
		time.Sleep(time.Millisecond * time.Duration(rand.IntN(13)+10))
	}
	for k := 0; k < 10; k++ {
		GoByID(k, func() {
			rp.Wait(ctx, 1, func(ctx context.Context) {
				calc()
			}, false)
			rp.Wait(ctx, 1, func(ctx context.Context) {
			}, false)
		})
	}
	GoByID(10000, func() {
		time.Sleep(time.Second * 10)
		t.Log("done")
	})
	wg.Wait()
}
