package workers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
)

type Fetcher interface {
	Queue() string
	Fetch()
	Acknowledge(context.Context, *Msg) error
	Ready() chan bool
	FinishedWork() chan bool
	Messages() chan *Msg
	Close()
	Closed() bool
}

type fetch struct {
	queue        string
	ready        chan bool
	finishedwork chan bool
	messages     chan *Msg
	stop         chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
	ctx          context.Context
	pool         *redis.Pool
	processID    string
}

func NewFetch(queue string, messages chan *Msg, ready chan bool) Fetcher {
	return &fetch{
		queue:        queue,
		ready:        ready,
		finishedwork: make(chan bool),
		messages:     messages,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		pool:         Config.Pool,
		processID:    Config.processId,
	}
}

func (f *fetch) Queue() string {
	return f.queue
}

func (f *fetch) processOldMessages() bool {
	messages := f.inprogressMessages()

	for _, message := range messages {
		select {
		case <-f.stop:
			return false
		case <-f.Ready():
		}
		if !f.sendMessage(message) {
			return false
		}
	}
	return true
}

func (f *fetch) Fetch() {
	defer close(f.done)
	if f.ctx == nil {
		f.ctx = context.Background()
	}
	if !f.processOldMessages() {
		return
	}

	for {
		select {
		case <-f.stop:
			return
		case <-f.Ready():
			f.tryFetchMessage()
		}
	}
}

func (f *fetch) tryFetchMessage() {
	conn, err := f.pool.GetContext(f.ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			Logger.Errorln("failed to get Redis connection", err)
		}
		return
	}
	defer conn.Close()

	message, err := redis.String(redis.DoContext(conn, f.ctx, "brpoplpush", f.queue, f.inprogressQueue(), 1))

	if err != nil {
		// If redis returns null, the queue is empty. Just ignore the error.
		if !errors.Is(err, redis.ErrNil) && !errors.Is(err, context.Canceled) {
			Logger.Errorln("failed to fetch message", err)
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-f.stop:
			case <-timer.C:
			}
		}
	} else {
		f.sendMessage(message)
	}
}

func (f *fetch) sendMessage(message string) bool {
	msg, err := NewMsg(message)

	if err != nil {
		Logger.Errorln("failed to create message from", message, ":", err)
		return true
	}

	select {
	case <-f.stop:
		return false
	case f.Messages() <- msg:
		return true
	}
}

func (f *fetch) Acknowledge(ctx context.Context, message *Msg) error {
	conn, err := f.pool.GetContext(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = redis.DoContext(conn, ctx, "lrem", f.inprogressQueue(), -1, message.OriginalJson())
	return err
}

func (f *fetch) Messages() chan *Msg {
	return f.messages
}

func (f *fetch) Ready() chan bool {
	return f.ready
}

func (f *fetch) FinishedWork() chan bool {
	return f.finishedwork
}

func (f *fetch) Close() {
	f.stopOnce.Do(func() {
		close(f.stop)
	})
	<-f.done
}

func (f *fetch) Closed() bool {
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

func (f *fetch) inprogressMessages() []string {
	conn, err := f.pool.GetContext(f.ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			Logger.Errorln("failed to get Redis connection", err)
		}
		return nil
	}
	defer conn.Close()

	messages, err := redis.Strings(redis.DoContext(conn, f.ctx, "lrange", f.inprogressQueue(), 0, -1))
	if err != nil && !errors.Is(err, context.Canceled) {
		Logger.Errorln("failed to fetch messages in progress", err)
	}

	return messages
}

func (f *fetch) inprogressQueue() string {
	return fmt.Sprint(f.queue, ":", f.processID, ":inprogress")
}
