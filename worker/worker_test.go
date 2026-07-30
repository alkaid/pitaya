package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/worker/mocks"
	"github.com/stretchr/testify/assert"
	workers "github.com/topfreegames/go-workers"
	"go.uber.org/mock/gomock"
)

const workerSubmissionErrorHelperEnv = "PITAYA_TEST_WORKER_SUBMISSION_ERROR"

type legacyWorkerStarter interface {
	Start()
}

var _ legacyWorkerStarter = (*Worker)(nil)

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

func TestParsedRPCJob(t *testing.T) {
	t.Parallel()

	var (
		mockRPCJob *mocks.MockRPCJob
		testErr    = errors.New("error")
		route      = "server.svc.method"
		serverID   = "serverid"
		ctx        = context.Background()
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
					RPC(ctx, serverID, route, &protos.BindMsg{}, &protos.BindMsg{Uid: "string"}).
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
					RPC(ctx, serverID, route, &protos.BindMsg{}, &protos.BindMsg{Uid: "string"})
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
