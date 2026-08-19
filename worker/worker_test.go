package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	workers "github.com/alkaid/pitaya/v2/internal/workers"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/worker/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

func TestNewWorkerRejectsUnreadableTLSBeforeConfiguringRuntime(t *testing.T) {
	workerConfig := config.NewDefaultPitayaConfig().Worker
	secretPath := filepath.Join(t.TempDir(), "tenant-secret-ca.pem")
	workerConfig.Redis.TLS = config.TLSConfig{Enabled: true, CAFile: secretPath}
	previousConfig := workers.Config

	worker, err := NewWorker(workerConfig, workerConfig.Retry)
	require.Error(t, err)
	assert.Nil(t, worker)
	assert.Contains(t, err.Error(), "pitaya.worker.redis.tls.cafile")
	assert.NotContains(t, err.Error(), secretPath)
	assert.Same(t, previousConfig, workers.Config)
	assert.NotContains(t, fmt.Sprint(err), secretPath)
}

const workerSubmissionErrorHelperEnv = "PITAYA_TEST_WORKER_SUBMISSION_ERROR"

type legacyWorkerStarter interface {
	Start()
}

var _ legacyWorkerStarter = (*Worker)(nil)

type capturingRPCJob struct {
	ctx context.Context
}

func (j *capturingRPCJob) ServerDiscovery(string, map[string]interface{}) (string, error) {
	return "server-1", nil
}

func (j *capturingRPCJob) RPC(ctx context.Context, _, _ string, _, _ proto.Message) error {
	j.ctx = ctx
	return nil
}

func (j *capturingRPCJob) GetArgReply(string) (proto.Message, proto.Message, error) {
	return &protos.BindMsg{}, &protos.BindMsg{}, nil
}

func TestStartPropagatesSubmissionError(t *testing.T) {
	if os.Getenv(workerSubmissionErrorHelperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStartPropagatesSubmissionError$")
		cmd.Env = append(os.Environ(), workerSubmissionErrorHelperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("worker submission helper failed: %v\n%s", err, output)
		}
		return
	}

	ants.Release()
	w := &Worker{}
	err := w.StartWithError()
	assert.ErrorIs(t, err, ants.ErrPoolClosed)
	assert.False(t, w.Started())

	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		w.Start()
	}()
	panicErr, ok := panicValue.(error)
	if !ok {
		t.Fatalf("panic = %v, want error", panicValue)
	}
	assert.ErrorIs(t, panicErr, ants.ErrPoolClosed)
	assert.False(t, w.Started())
}

func TestStopCancelsLifecycleWhileJobsReadContext(t *testing.T) {
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	w := &Worker{lifecycle: lifecycleCtx, cancel: cancel}
	w.started.Store(true)
	contextRead := make(chan struct{})
	w.quit = func() {
		_ = w.context()
		close(contextRead)
	}
	observedCtx := w.context()
	stopDone := make(chan struct{})
	go func() {
		w.Stop()
		close(stopDone)
	}()

	select {
	case <-observedCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the worker lifecycle context")
	}
	select {
	case <-contextRead:
	case <-time.After(time.Second):
		t.Fatal("concurrent worker context read deadlocked with Stop")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for the worker runtime to exit")
	}
	assert.False(t, w.Started())
	assert.ErrorIs(t, w.RegisterRPCJob(nil), constants.ErrWorkerStopped)
}

func TestStopClearsRegisteredRuntimeBeforeStart(t *testing.T) {
	quitCalled := false
	w := &Worker{
		registered: true,
		quit: func() {
			quitCalled = true
		},
	}

	w.Stop()

	assert.True(t, quitCalled)
	assert.False(t, w.registered)
	assert.ErrorIs(t, w.RegisterRPCJob(nil), constants.ErrWorkerStopped)
}

func TestParsedRPCJob(t *testing.T) {
	t.Parallel()

	var (
		mockRPCJob *mocks.MockRPCJob
		testErr    = errors.New("error")
		route      = "server.svc.method"
		serverID   = "serverid"
	)

	tables := map[string]struct {
		arg    *workers.Msg
		mocks  func()
		assert func(func(*workers.Msg), *workers.Msg)
	}{
		"test_error_when_route_not_string": {
			arg: func() *workers.Msg {
				msg, err := workers.NewMsg(`{"args": {"route": 10}}`)
				assert.NoError(t, err)
				return msg
			}(),
			mocks: func() {},
			assert: func(f func(*workers.Msg), m *workers.Msg) {
				assert.Panics(t, func() { f(m) })
			},
		},
		"test_error_on_get_arg_reply": {
			arg: func() *workers.Msg {
				msg, err := workers.NewMsg(`{
					"args": { "route": "server.svc.method" }
				}`)
				assert.NoError(t, err)
				return msg
			}(),
			mocks: func() {
				mockRPCJob.EXPECT().
					GetArgReply(route).
					Return(nil, nil, testErr)
			},
			assert: func(f func(*workers.Msg), m *workers.Msg) {
				assert.Panics(t, func() { f(m) })
			},
		},
		"test_error_on_unmarshal_rpc_info": {
			arg: func() *workers.Msg {
				msg, err := workers.NewMsg(`{
					"args": { 
						"route": "server.svc.method",
						"arg": { "uid": 10 }
					}
				}`)
				assert.NoError(t, err)
				return msg
			}(),
			mocks: func() {
				mockRPCJob.EXPECT().
					GetArgReply(route).
					Return(&protos.BindMsg{}, &protos.BindMsg{}, nil)
			},
			assert: func(f func(*workers.Msg), m *workers.Msg) {
				assert.Panics(t, func() { f(m) })
			},
		},
		"test_error_on_server_discovery": {
			arg: func() *workers.Msg {
				msg, err := workers.NewMsg(`{
					"args": { 
						"route": "server.svc.method",
						"arg": { "uid": "string" },
						"metadata": { "stack": "a" }
					}
				}`)
				assert.NoError(t, err)
				return msg
			}(),
			mocks: func() {
				mockRPCJob.EXPECT().
					GetArgReply(route).
					Return(&protos.BindMsg{}, &protos.BindMsg{}, nil)
				mockRPCJob.EXPECT().
					ServerDiscovery(route, map[string]interface{}{"stack": "a"}).
					Return("", testErr)
			},
			assert: func(f func(*workers.Msg), m *workers.Msg) {
				assert.Panics(t, func() { f(m) })
			},
		},
		"test_error_on_rpc": {
			arg: func() *workers.Msg {
				msg, err := workers.NewMsg(`{
					"args": { 
						"route": "server.svc.method",
						"arg": { "uid": "string" },
						"metadata": { "stack": "a" }
					}
				}`)
				assert.NoError(t, err)
				return msg
			}(),
			mocks: func() {
				mockRPCJob.EXPECT().
					GetArgReply(route).
					Return(&protos.BindMsg{}, &protos.BindMsg{}, nil)
				mockRPCJob.EXPECT().
					ServerDiscovery(route, map[string]interface{}{"stack": "a"}).
					Return(serverID, nil)
				mockRPCJob.EXPECT().
					RPC(gomock.Any(), serverID, route, &protos.BindMsg{}, &protos.BindMsg{Uid: "string"}).
					Return(testErr)
			},
			assert: func(f func(*workers.Msg), m *workers.Msg) {
				assert.Panics(t, func() { f(m) })
			},
		},
		"test_execute_rpc": {
			arg: func() *workers.Msg {
				msg, err := workers.NewMsg(`{
					"args": { 
						"route": "server.svc.method",
						"arg": { "uid": "string" },
						"metadata": { "stack": "a" }
					}
				}`)
				assert.NoError(t, err)
				return msg
			}(),
			mocks: func() {
				mockRPCJob.EXPECT().
					GetArgReply(route).
					Return(&protos.BindMsg{}, &protos.BindMsg{}, nil)
				mockRPCJob.EXPECT().
					ServerDiscovery(route, map[string]interface{}{"stack": "a"}).
					Return(serverID, nil)
				mockRPCJob.EXPECT().
					RPC(gomock.Any(), serverID, route, &protos.BindMsg{}, &protos.BindMsg{Uid: "string"})
			},
			assert: func(f func(*workers.Msg), m *workers.Msg) {
				assert.NotPanics(t, func() { f(m) })
			},
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRPCJob = mocks.NewMockRPCJob(ctrl)
			table.mocks()

			worker := &Worker{}
			table.assert(worker.parsedRPCJob(mockRPCJob), table.arg)
		})
	}
}

func TestParsedRPCJobPropagatesStableJobIDAsIdempotencyKey(t *testing.T) {
	msg, err := workers.NewMsg(`{
		"jid": "job-stable-123",
		"args": {
			"route": "server.svc.method",
			"arg": {"uid": "user-1"},
			"metadata": {}
		}
	}`)
	assert.NoError(t, err)

	job := &capturingRPCJob{}
	assert.NotPanics(t, func() { (&Worker{}).parsedRPCJob(job)(msg) })
	assert.Equal(t, "job-stable-123", pcontext.GetFromPropagateCtx(job.ctx, constants.IdempotencyKey))
}
