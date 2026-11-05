package co

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
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
	t.Cleanup(p.pool.Release)
	var wg sync.WaitGroup
	var completed atomic.Int64
	ctx := context.Background()
	const rootCount = 1000
	// 各层使用互不重叠的 ID，避免有界队列满后出现自投递或循环等待。
	const branchIDBase = rootCount
	const leafIDBase = branchIDBase + rootCount + 3
	var GoByID = func(goID int, task func()) {
		wg.Add(1)
		p.Go(ctx, goID, func(ctx context.Context) {
			defer wg.Done()
			task()
			completed.Add(1)
		}, true) // 压测只验证任务派发与完成，避免超时监控额外创建大量 goroutine。
	}
	var calc = func() {
		// 模拟一个耗时操作
		time.Sleep(time.Microsecond * time.Duration(rand.IntN(1000)))
	}
	done := make(chan struct{})
	go func() {
		// 先填满中间层的物理队列，再允许它派发叶子任务。
		// 若两层再次复用 ID，这一步会稳定暴露自投递阻塞，而不依赖随机调度。
		started := make(chan struct{})
		resume := make(chan struct{})
		GoByID(branchIDBase+1, func() {
			close(started)
			<-resume
			GoByID(leafIDBase+1, calc)
		})
		<-started
		for i := 0; i < 2*p.config.TaskBuffer; i++ {
			GoByID(branchIDBase+1, calc)
		}
		close(resume)
		for k := 0; k < rootCount; k++ {
			GoByID(k, func() {
				GoByID(branchIDBase+k+1, func() {
					for i := 0; i < 5; i++ {
						GoByID(leafIDBase+i, calc)
					}
					calc()
				})
				GoByID(branchIDBase+k+2, func() {
					for i := 0; i < 5; i++ {
						GoByID(leafIDBase+i, calc)
					}
					calc()
				})
				GoByID(branchIDBase+k+3, func() {
					for i := 0; i < 5; i++ {
						GoByID(leafIDBase+i, calc)
					}
					calc()
				})
				calc()
			})
		}
		wg.Wait()
		close(done)
	}()
	// 在默认的五分钟运行超时替换 worker 前失败，防止恢复机制掩盖循环等待。
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("nested tasks did not finish: completed %d tasks", completed.Load())
	}
	expectedTasks := int64(rootCount*(1+3*(1+5)) + 2*p.config.TaskBuffer + 2)
	if got := completed.Load(); got != expectedTasks {
		t.Fatalf("completed %d tasks, want %d", got, expectedTasks)
	}
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
