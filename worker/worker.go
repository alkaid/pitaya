// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	workers "github.com/alkaid/pitaya/v2/internal/workers"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/logger/interfaces"
	"google.golang.org/protobuf/proto"
)

// Worker executes RPCs with retry and backoff time
type Worker struct {
	concurrency int
	registered  bool
	opts        *config.EnqueueOpts
	startStopMu sync.Mutex
	lifecycleMu sync.RWMutex
	lifecycle   context.Context
	cancel      context.CancelFunc
	started     atomic.Bool
	stopped     bool
	quit        func()
}

// NewWorker configures and returns a *Worker
func NewWorker(workerConfig config.WorkerConfig, opts config.EnqueueOpts) (*Worker, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}

	poolSize, err := strconv.Atoi(workerConfig.Redis.Pool)
	if err != nil {
		return nil, err
	}
	clientTLS, err := config.LoadClientTLS("pitaya.worker.redis.tls", workerConfig.Redis.TLS)
	if err != nil {
		return nil, err
	}
	dialOptions := make([]redis.DialOption, 0, 2)
	if clientTLS != nil {
		dialOptions = append(
			dialOptions,
			redis.DialUseTLS(true),
			redis.DialTLSConfig(clientTLS),
		)
	}

	workers.Configure(workers.Options{
		Address:     workerConfig.Redis.ServerURL,
		Username:    workerConfig.Redis.Username,
		Password:    workerConfig.Redis.Password,
		Namespace:   workerConfig.Namespace,
		ProcessID:   hostname,
		PoolSize:    poolSize,
		DialOptions: dialOptions,
	})

	return &Worker{
		concurrency: workerConfig.Concurrency,
		opts:        &opts,
		quit:        workers.QuitAndReset,
	}, nil
}

// SetLogger overwrites worker logger
func (w *Worker) SetLogger(logger interfaces.Logger) {
	workers.SetLogger(logger)
}

// Start starts worker in another goroutine and panics if it cannot be admitted.
func (w *Worker) Start() {
	if err := w.StartWithError(); err != nil {
		panic(err)
	}
}

// StartWithError starts worker in another goroutine and reports admission errors.
func (w *Worker) StartWithError() error {
	w.startStopMu.Lock()
	defer w.startStopMu.Unlock()
	if w.started.Load() {
		return nil
	}
	w.lifecycleMu.Lock()
	w.lifecycle, w.cancel = context.WithCancel(context.Background())
	w.lifecycleMu.Unlock()
	started := make(chan struct{})
	if err := co.Submit(func() {
		defer close(started)
		workers.Start()
	}); err != nil {
		w.lifecycleMu.RLock()
		cancel := w.cancel
		w.lifecycleMu.RUnlock()
		cancel()
		return fmt.Errorf("start worker: %w", err)
	}
	<-started
	w.started.Store(true)
	w.stopped = false
	return nil
}

// Stop stops accepting worker jobs, cancels the context passed to an active RPC
// job, and waits for the go-workers runtime to exit.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.startStopMu.Lock()
	defer w.startStopMu.Unlock()
	w.lifecycleMu.RLock()
	cancel := w.cancel
	w.lifecycleMu.RUnlock()
	if cancel != nil {
		cancel()
	}
	w.stopped = true
	if w.started.Swap(false) || w.registered {
		quit := w.quit
		if quit == nil {
			quit = workers.QuitAndReset
		}
		quit()
	}
	w.registered = false
}

// Started returns true if worker was started
func (w *Worker) Started() bool {
	return w != nil && w.started.Load()
}

func (w *Worker) context() context.Context {
	w.lifecycleMu.RLock()
	defer w.lifecycleMu.RUnlock()
	if w.lifecycle == nil {
		return context.Background()
	}
	return w.lifecycle
}

// EnqueueRPC enqueues rpc job to worker
func (w *Worker) EnqueueRPC(
	routeStr string,
	metadata map[string]interface{},
	reply, arg proto.Message,
) (jid string, err error) {
	opts := w.enqueueOptions(w.opts)
	return workers.EnqueueWithOptions(rpcQueue, class, &rpcInfo{
		Route:    routeStr,
		Metadata: metadata,
		Arg:      arg,
		Reply:    reply,
	}, opts)
}

// EnqueueRPCWithOptions enqueues rpc job to worker
func (w *Worker) EnqueueRPCWithOptions(
	routeStr string,
	metadata map[string]interface{},
	reply, arg proto.Message,
	opts *config.EnqueueOpts,
) (jid string, err error) {
	return workers.EnqueueWithOptions(rpcQueue, class, &rpcInfo{
		Route:    routeStr,
		Metadata: metadata,
		Arg:      arg,
		Reply:    reply,
	}, w.enqueueOptions(opts))
}

// RegisterRPCJob registers a RPC job
func (w *Worker) RegisterRPCJob(rpcJob RPCJob) error {
	w.startStopMu.Lock()
	defer w.startStopMu.Unlock()
	if w.stopped {
		return constants.ErrWorkerStopped
	}
	if w.registered {
		return constants.ErrRPCJobAlreadyRegistered
	}

	job := w.parsedRPCJob(rpcJob)
	workers.Process(rpcQueue, job, w.concurrency)
	w.registered = true
	return nil
}

func (w *Worker) parsedRPCJob(rpcJob RPCJob) func(*workers.Msg) {
	return func(jobArg *workers.Msg) {
		logger.Log.Debug("executing rpc job")
		bts, rpcRoute, err := w.unmarshalRouteMetadata(jobArg)
		if err != nil {
			logger.Zap.Error("failed to get job arg", zap.Error(err))
			panic(err)
		}

		logger.Log.Debug("getting route arg and reply")
		arg, reply, err := rpcJob.GetArgReply(rpcRoute.Route)
		if err != nil {
			logger.Zap.Error("failed to get methods arg and reply", zap.Error(err))
			panic(err)
		}
		rpcInfo := &rpcInfo{
			Arg:   arg,
			Reply: reply,
		}

		logger.Log.Debug("unmarshalling rpc info")
		err = json.Unmarshal(bts, rpcInfo)
		if err != nil {
			logger.Zap.Error("failed to unmarshal rpc info", zap.Error(err))
			panic(err)
		}

		logger.Log.Debug("choosing server to make rpc")
		serverID, err := rpcJob.ServerDiscovery(rpcInfo.Route, rpcInfo.Metadata)
		if err != nil {
			logger.Zap.Error("failed get server", zap.Error(err))
			panic(err)
		}

		ctx := pcontext.AddToPropagateCtx(w.context(), constants.IdempotencyKey, jobArg.Jid())

		logger.Log.Debugf("executing rpc func to %s", rpcInfo.Route)
		err = rpcJob.RPC(ctx, serverID, rpcInfo.Route, reply, arg)
		if err != nil {
			logger.Zap.Error("failed make rpc", zap.Error(err))
			panic(err)
		}

		logger.Log.Debug("finished executing rpc job")
	}
}

func (w *Worker) enqueueOptions(
	opts *config.EnqueueOpts,
) workers.EnqueueOptions {
	return workers.EnqueueOptions{
		Retry:    opts.Enabled,
		RetryMax: opts.Max,
		RetryOptions: workers.RetryOptions{
			Exp:      opts.Exponential,
			MinDelay: opts.MinDelay,
			MaxDelay: opts.MaxDelay,
			MaxRand:  opts.MaxRandom,
		},
	}
}

func (w *Worker) unmarshalRouteMetadata(
	jobArg *workers.Msg,
) ([]byte, *rpcRoute, error) {
	bts, err := jobArg.Args().MarshalJSON()
	if err != nil {
		return nil, nil, err
	}

	rpcRoute := new(rpcRoute)
	err = json.Unmarshal(bts, rpcRoute)
	if err != nil {
		return nil, nil, err
	}

	return bts, rpcRoute, nil
}
