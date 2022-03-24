package co

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/topfreegames/pitaya/v2/logger"
)

func TestMain(m *testing.M) {
	logger.Manager.SetDevelopment(true)
	logger.Manager.SetLevel("info")
	logger.Zap = logger.Manager.Log
	logger.Sugar = logger.Manager.Sugar
	defer logger.Zap.Sync()
	exit := m.Run()
	os.Exit(exit)
}

func BenchmarkGoroutine_Async(b *testing.B) {
	wg := &sync.WaitGroup{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		// 消息处理
		context.Background()
		go func(i int) {
			wg.Done()
		}(i)
	}
	wg.Wait()
}

func BenchmarkLooper_Async(b *testing.B) {
	//  cd co && go test -race -bench=. -benchtime=10s
	l := NewLooper(0, b.N*10)
	l.Start()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 消息处理
		ctx := context.Background()
		l.Async(ctx, func(ctx context.Context, co Coroutine) (any, error) {
			return "", nil
		}).Wait(ctx)
	}
	l.Exit()
}

// BenchmarkGoroutine_Await 同样一段处理逻辑比较原生goroutine和自定义coroutine的基准测试结果 对比 BenchmarkLooper_Await
//  @param b
func BenchmarkGoroutine_Await(b *testing.B) {
	//  cd co && go test -bench=. -benchtime=100000x
	wg := &sync.WaitGroup{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		// 消息处理
		go func(i int) {
			context.Background()
			chResp := make(chan string)
			go func(i int) {
				chDb := make(chan string)
				go func(i int) {
					chDb <- fmt.Sprintf("dbsuccess%d", i)
				}(i)
				// t.Log("msgco end:", i, " coid:", co.GetID(), " ", dbRet)
				chResp <- fmt.Sprintf("resp %d, %s", i, <-chDb)
			}(i)
			fmt.Sprintf("msg end %d %s", i, <-chResp)
			wg.Done()
		}(i)
	}
	wg.Wait()
}

func BenchmarkLooper_Await(b *testing.B) {
	//  cd co && go test -bench=. -benchtime=10s
	l := NewLooper(0, b.N*10)
	l.Start()
	wg := &sync.WaitGroup{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		// 消息处理
		go func(i int) {
			// t.Log("msggo begin:", i, " coid:")
			ctx := context.Background()
			resp, _ := l.Async(ctx, func(ctx context.Context, co Coroutine) (any, error) {
				// t.Log("msgco begin:", i, " coid:", co.GetID())
				var dbRet any
				for k := 0; k < 10; k++ {
					dbRet, _ = co.Await(ctx, func(ctx context.Context, co Coroutine) (any, error) {
						// t.Log("db begin:", i, " coid:", co.GetID())
						// time.Sleep(time.Millisecond * time.Duration(rand.Int31n(500)+1))
						// t.Log("db end:", i, " coid:", co.GetID())
						return fmt.Sprintf("dbsuccess%d", i), nil
					})
				}
				// t.Log("msgco end:", i, " coid:", co.GetID(), " ", dbRet)
				return fmt.Sprintf("resp %d, %s", i, dbRet), nil
			}).Wait(ctx)
			fmt.Sprintf("msg end %d %s", i, resp)
			// t.Log("msg end: ", i, " ", resp)
			wg.Done()
		}(i)
	}
	wg.Wait()
}

func TestLooper_Async(t *testing.T) {
	// test race
	// go test -race github.com/topfreegames/pitaya/v2/co
	n := 1
	l := NewLooper(0, n*2)
	l.Start()
	wg := &sync.WaitGroup{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		// 消息处理
		go func(i int) {
			// t.Log("msggo begin:", i, " coid:")
			ctx := context.Background()
			resp, _ := l.Async(ctx, func(ctx context.Context, co Coroutine) (any, error) {
				// t.Log("msgco begin:", i, " coid:", co.GetID())
				dbRet, _ := co.Await(ctx, func(ctx context.Context, co Coroutine) (any, error) {
					// t.Log("db begin:", i, " coid:", co.GetID())
					// time.Sleep(time.Millisecond * time.Duration(rand.Int31n(500)+1))
					// t.Log("db end:", i, " coid:", co.GetID())
					return fmt.Sprintf("dbsuccess%d", i), nil
				})
				dbRet, _ = co.Await(ctx, func(ctx context.Context, co Coroutine) (any, error) {
					// t.Log("db begin:", i, " coid:", co.GetID())
					// time.Sleep(time.Millisecond * time.Duration(rand.Int31n(500)+1))
					// t.Log("db end:", i, " coid:", co.GetID())
					return fmt.Sprintf("dbsuccess%d", i), nil
				})
				// t.Log("msgco end:", i, " coid:", co.GetID(), " ", dbRet)
				return fmt.Sprintf("resp %d, %s", i, dbRet), nil
			}).Wait(ctx)
			fmt.Sprintf("msg end %d %s", i, resp)
			t.Log("msg end: ", i, " ", resp)
			wg.Done()
		}(i)
	}
	wg.Wait()
	l.Exit()
}

//
// func TestCrossGoroutine(t *testing.T) {
// 	l0 := NewLooper(0, 1000)
// 	l1 := NewLooper(1, 1000)
// 	l0.Start()
// 	l1.Start()
// 	s3 := NewLooper(3, 1000)
// 	t4 := NewLooper(4, 1000)
// 	defer func() {
// 		l0.Exit()
// 		l1.Exit()
// 		s3.Exit()
// 		t4.Exit()
// 	}()
// 	l0.Async(context.Background(), func(ctx context.Context, co Coroutine) {
// 		meta := fmt.Sprintf("lid:%d,cid:%d", co.GetGoroutineID(), co.GetID())
// 		t.Log(meta, "l0 recv msg begin")
// 		l1.Async(ctx, func(ctx context.Context, co2 Coroutine) {
// 			meta := fmt.Sprintf("lid:%d,cid:%d", co2.GetGoroutineID(), co2.GetID())
// 			t.Log(meta, "l1 recv msg beigin")
// 			// 调l1的await
// 			ret, _ := co.Await(func(ctx context.Context, co Coroutine) (any, error) {
// 				meta := fmt.Sprintf("lid:%d,cid:%d", co.GetGoroutineID(), co.GetID())
// 				t.Log(meta, "l0 proc db begin")
// 				time.Sleep(time.Second)
// 				t.Log(meta, "l0 proc db end")
// 				return "3344", nil
// 			})
// 			t.Log(meta, "l1 recv msg end. ret:", ret)
// 		})
// 		t.Log(meta, "l0 recv msg end")
// 	})
// 	// 每个uid或session一个线程
// 	// go func() {
// 	// 	// 收到消息
// 	// 	msg:="msg1000"
// 	// 	uid:=3
// 	// 	ret:= func(msg string) string{
// 	// 		return msg
// 	// 	}(msg)
// 	// }()
// }
