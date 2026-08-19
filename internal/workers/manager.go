package workers

import (
	"context"
	"errors"
	"strings"
	"sync"
)

type manager struct {
	queue       string
	fetch       Fetcher
	job         jobFunc
	concurrency int
	workers     []*worker
	workersM    *sync.Mutex
	confirm     chan *confirmation
	stop        chan struct{}
	done        chan struct{}
	quitOnce    sync.Once
	mids        *Middlewares
	runtimeCtx  context.Context
	ackWG       sync.WaitGroup
}

type confirmation struct {
	message *Msg
	done    chan struct{}
}

func (m *manager) start(ctx context.Context) {
	m.stop = make(chan struct{})
	m.done = make(chan struct{})
	m.confirm = make(chan *confirmation)
	m.quitOnce = sync.Once{}
	m.runtimeCtx = ctx
	m.ackWG = sync.WaitGroup{}
	if f, ok := m.fetch.(*fetch); ok {
		f.ctx = ctx
	}
	m.loadWorkers()
	go m.manage()
}

func (m *manager) prepare() {
	if !m.fetch.Closed() {
		m.fetch.Close()
	}
}

func (m *manager) quit() {
	m.quitOnce.Do(func() {
		Logger.Infoln("quitting queue", m.queueName(), "(waiting for", m.processing(), "/", len(m.workers), "workers).")
		m.prepare()

		m.workersM.Lock()
		workers := append([]*worker(nil), m.workers...)
		m.workersM.Unlock()
		for _, worker := range workers {
			worker.stopWork()
		}
		for _, worker := range workers {
			worker.wait()
		}

		close(m.stop)
		<-m.done
		m.reset()
	})
}

func (m *manager) manage() {
	defer close(m.done)
	Logger.Infoln("processing queue", m.queueName(), "with", m.concurrency, "workers.")

	go m.fetch.Fetch()

	for {
		select {
		case confirmed := <-m.confirm:
			m.ackWG.Add(1)
			go func(confirmation *confirmation) {
				defer m.ackWG.Done()
				defer close(confirmation.done)
				m.acknowledge(confirmation.message)
			}(confirmed)
		case <-m.stop:
			m.ackWG.Wait()
			return
		}
	}
}

func (m *manager) acknowledge(message *Msg) {
	err := m.fetch.Acknowledge(m.runtimeCtx, message)
	if err == nil || m.runtimeCtx.Err() == nil {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
	defer cancel()
	if err := m.fetch.Acknowledge(cleanupCtx, message); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		Logger.Errorln("failed to acknowledge completed job during shutdown", err)
	}
}

func (m *manager) loadWorkers() {
	m.workersM.Lock()
	for i := 0; i < m.concurrency; i++ {
		m.workers[i] = newWorker(m)
		m.workers[i].start()
	}
	m.workersM.Unlock()
}

func (m *manager) processing() (count int) {
	m.workersM.Lock()
	for _, worker := range m.workers {
		if worker != nil && worker.processing() {
			count++
		}
	}
	m.workersM.Unlock()
	return
}

func (m *manager) queueName() string {
	return strings.Replace(m.queue, "queue:", "", 1)
}

func (m *manager) reset() {
	m.fetch = Config.Fetch(m.queue)
}

func newManager(queue string, job jobFunc, concurrency int, mids ...Action) *manager {
	var customMids *Middlewares
	if len(mids) == 0 {
		customMids = Middleware
	} else {
		customMids = NewMiddleware(Middleware.actions...)
		for _, m := range mids {
			customMids.Append(m)
		}
	}
	m := &manager{
		queue:       Config.Namespace + "queue:" + queue,
		job:         job,
		concurrency: concurrency,
		workers:     make([]*worker, concurrency),
		workersM:    &sync.Mutex{},
		confirm:     make(chan *confirmation),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		mids:        customMids,
	}

	m.fetch = Config.Fetch(m.queue)

	return m
}
