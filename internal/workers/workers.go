package workers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

const (
	RETRY_KEY          = "goretry"
	SCHEDULED_JOBS_KEY = "schedule"
)

var managers = make(map[string]*manager)
var schedule *scheduled
var control = make(map[string]chan string)
var access sync.RWMutex
var started bool
var stopping bool
var runtimeDone chan struct{}
var shutdownDone chan struct{}
var runtimeCtx context.Context
var runtimeCancel context.CancelFunc
var workerCleanupTimeout = redisReadTimeout

var Middleware = NewMiddleware(
	&MiddlewareLogging{},
	&MiddlewareRetry{},
	&MiddlewareStats{},
)

func Process(queue string, job jobFunc, concurrency int, mids ...Action) {
	lockAccess()
	defer access.Unlock()

	manager := newManager(queue, job, concurrency, mids...)
	managers[queue] = manager
	if started {
		manager.start(runtimeCtx)
	}
}

func Run() {
	Start()
	access.RLock()
	done := runtimeDone
	access.RUnlock()
	go handleSignals()
	<-done
}

func ResetManagers() error {
	lockAccess()
	defer access.Unlock()

	if started {
		return errors.New("Cannot reset worker managers while workers are running")
	}

	managers = make(map[string]*manager)

	return nil
}

func Start() {
	lockAccess()
	defer access.Unlock()

	if started {
		return
	}

	runHooks(beforeStart)
	runtimeDone = make(chan struct{})
	runtimeCtx, runtimeCancel = context.WithCancel(context.Background())
	startSchedule(runtimeCtx)
	startManagers(runtimeCtx)

	started = true
}

func Quit() {
	quit(false)
}

// QuitAndReset stops the runtime and atomically drops all registered managers.
// It is used by Pitaya so a subsequent App cannot restart managers that still
// reference the previous App's RPC job.
func QuitAndReset() {
	quit(true)
}

func quit(resetManagers bool) {
	for {
		access.Lock()
		if stopping {
			done := shutdownDone
			access.Unlock()
			<-done
			if resetManagers {
				continue
			}
			return
		}

		if !started {
			if resetManagers {
				managers = make(map[string]*manager)
			}
			access.Unlock()
			return
		}

		stopping = true
		shutdownDone = make(chan struct{})
		if runtimeCancel != nil {
			runtimeCancel()
		}
		managersToStop := make([]*manager, 0, len(managers))
		for _, manager := range managers {
			managersToStop = append(managersToStop, manager)
		}
		scheduleToStop := schedule
		runtimeDoneToClose := runtimeDone
		access.Unlock()

		managerDone := quitManagers(managersToStop)
		if scheduleToStop != nil {
			scheduleToStop.stop()
		}
		runHooks(duringDrain)
		managerDone.Wait()
		if scheduleToStop != nil {
			scheduleToStop.wait()
		}

		access.Lock()
		schedule = nil
		started = false
		runtimeCtx = nil
		runtimeCancel = nil
		if runtimeDoneToClose != nil {
			close(runtimeDoneToClose)
		}
		if resetManagers {
			managers = make(map[string]*manager)
		}
		stopping = false
		close(shutdownDone)
		access.Unlock()
		return
	}
}

func StatsServer(port int) {
	http.HandleFunc("/stats", Stats)

	Logger.Infoln("Stats are available at", fmt.Sprint("http://localhost:", port, "/stats"))

	if err := http.ListenAndServe(fmt.Sprint(":", port), nil); err != nil {
		Logger.Error("failed to start stats server", err)
	}
}

func startSchedule(ctx context.Context) {
	if schedule == nil {
		schedule = newScheduled(RETRY_KEY, SCHEDULED_JOBS_KEY)
	}

	schedule.start(ctx, Config.Pool, Config.Namespace, Config.PoolInterval)
}

func startManagers(ctx context.Context) {
	for _, manager := range managers {
		manager.start(ctx)
	}
}

func quitManagers(managers []*manager) *sync.WaitGroup {
	var done sync.WaitGroup
	done.Add(len(managers))
	for _, m := range managers {
		go func(m *manager) {
			defer done.Done()
			m.quit()
		}(m)
	}
	return &done
}

func lockAccess() {
	for {
		access.Lock()
		if !stopping {
			return
		}
		done := shutdownDone
		access.Unlock()
		<-done
	}
}
