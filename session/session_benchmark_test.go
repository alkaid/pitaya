package session

import (
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
)

func BenchmarkSessionHotReads(b *testing.B) {
	const sessionCount = 256

	pool := NewSessionPool().(*sessionPoolImpl)
	sessions := make([]Session, sessionCount)
	uids := make([]string, sessionCount)
	for i := range sessions {
		uids[i] = strconv.Itoa(i + 1)
		sessions[i], _ = pool.NewSession(nil, true, uids[i])
		if err := pool.StoreSessionLocal(sessions[i]); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("UIDInt/serial", func(b *testing.B) {
		b.ReportAllocs()
		var got int64
		for i := 0; i < b.N; i++ {
			got = sessions[0].UIDInt()
		}
		runtime.KeepAlive(got)
	})

	b.Run("UIDInt/parallel-different-sessions", func(b *testing.B) {
		b.ReportAllocs()
		var worker atomic.Uint64
		b.RunParallel(func(pb *testing.PB) {
			sess := sessions[int(worker.Add(1)-1)%len(sessions)]
			var got int64
			for pb.Next() {
				got = sess.UIDInt()
			}
			runtime.KeepAlive(got)
		})
	})

	b.Run("GetSessionByUID/serial", func(b *testing.B) {
		b.ReportAllocs()
		var got Session
		for i := 0; i < b.N; i++ {
			got = pool.GetSessionByUID(uids[0])
		}
		runtime.KeepAlive(got)
	})

	b.Run("GetSessionByUID/parallel-different-users", func(b *testing.B) {
		b.ReportAllocs()
		var worker atomic.Uint64
		b.RunParallel(func(pb *testing.PB) {
			uid := uids[int(worker.Add(1)-1)%len(uids)]
			var got Session
			for pb.Next() {
				got = pool.GetSessionByUID(uid)
			}
			runtime.KeepAlive(got)
		})
	})
}
