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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/gomodule/redigo/redis"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/internal/rpccontext"
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
		metadataDigest, err := rpcJobMetadataDigest(bts)
		if err != nil {
			logger.Zap.Error("failed to fingerprint RPC job metadata", zap.Error(err))
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

		jobID := jobArg.Jid()
		ctx := pcontext.AddToPropagateCtx(w.context(), constants.IdempotencyKey, jobID)
		ctx = rpccontext.WithReliableRPCJob(ctx, jobID, metadataDigest)

		logger.Log.Debugf("executing rpc func to %s", rpcInfo.Route)
		err = rpcJob.RPC(ctx, serverID, rpcInfo.Route, reply, arg)
		if err != nil {
			logger.Zap.Error("failed make rpc", zap.Error(err))
			panic(err)
		}

		logger.Log.Debug("finished executing rpc job")
	}
}

func rpcJobMetadataDigest(args []byte) (string, error) {
	var job struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(args, &job); err != nil {
		return "", err
	}
	metadata := job.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("null")
	}
	if !utf8.Valid(metadata) {
		return "", errors.New("RPC job metadata is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.UseNumber()
	var values any
	if err := decoder.Decode(&values); err != nil {
		return "", err
	}
	if values != nil {
		if _, ok := values.(map[string]any); !ok {
			return "", fmt.Errorf("RPC job metadata must be an object or null, got %T", values)
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", errors.New("RPC job metadata has a trailing JSON value")
	}
	canonical, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
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
	var envelope struct {
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal([]byte(jobArg.OriginalJson()), &envelope); err != nil {
		return nil, nil, err
	}
	if len(envelope.Args) == 0 {
		return nil, nil, errors.New("RPC job args are missing")
	}
	bts := append([]byte(nil), envelope.Args...)

	rpcRoute := new(rpcRoute)
	err := json.Unmarshal(bts, rpcRoute)
	if err != nil {
		return nil, nil, err
	}

	return bts, rpcRoute, nil
}
