package co

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func TestLooper_Async(t *testing.T) {
	l := NewLooper(0, 1000)

	l.Start()

	recvProc(l)

	l.Exit()
}

func TestCrossGoroutine(t *testing.T) {
	l0 := NewLooper(0, 1000)
	l1 := NewLooper(1, 1000)
	l0.Start()
	l1.Start()
	s3 := NewLooper(3, 1000)
	t4 := NewLooper(4, 1000)
	defer func() {
		l0.Exit()
		l1.Exit()
		s3.Exit()
		t4.Exit()
	}()
	l0.Async(context.Background(), func(ctx context.Context, co Coroutine) {
		meta := fmt.Sprintf("lid:%d,cid:%d", co.GetGoroutineID(), co.GetID())
		t.Log(meta, "l0 recv msg begin")
		l1.Async(ctx, func(ctx context.Context, co2 Coroutine) {
			meta := fmt.Sprintf("lid:%d,cid:%d", co2.GetGoroutineID(), co2.GetID())
			t.Log(meta, "l1 recv msg beigin")
			// 调l1的await
			ret, _ := co.Await(func(ctx context.Context, co Coroutine) (any, error) {
				meta := fmt.Sprintf("lid:%d,cid:%d", co.GetGoroutineID(), co.GetID())
				t.Log(meta, "l0 proc db begin")
				time.Sleep(time.Second)
				t.Log(meta, "l0 proc db end")
				return "3344", nil
			})
			t.Log(meta, "l1 recv msg end. ret:", ret)
		})
		t.Log(meta, "l0 recv msg end")
	})
	// 每个uid或session一个线程
	// go func() {
	// 	// 收到消息
	// 	msg:="msg1000"
	// 	uid:=3
	// 	ret:= func(msg string) string{
	// 		return msg
	// 	}(msg)
	// }()
}

var data = make(map[string]int32)

// 消息处理
func msgProc(ctx context.Context, co Coroutine) {
	fmt.Println("msgProc begin:", ctx.Value("msgid").(int), " coid:", co.GetID())
	ret, _ := co.Await(dbProc)
	data["tmp"] = ret.(int32)
	fmt.Println("msgProc end:", ctx.Value("msgid").(int), " ret", ret, " coid:", co.GetID())
}

// db耗时处理
func dbProc(ctx context.Context, co Coroutine) (any, error) {

	msgid := ctx.Value("msgid").(int)

	du := rand.Int31n(5) + 1
	time.Sleep(time.Duration(du) * time.Second)
	fmt.Println(msgid, "dbProc", " ret", du, " coid:", co.GetID())
	return du, nil
}

// 消息线程, 生成任务
func recvProc(l *Looper) {

	for i := 0; i < 100; i++ {

		l.Async(context.WithValue(context.Background(), "msgid", i), msgProc)

		fmt.Println(i, "recv Msg")

	}

}
