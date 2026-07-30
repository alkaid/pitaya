package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/alkaid/pitaya/v2/route"
	protobufserializer "github.com/alkaid/pitaya/v2/serialize/protobuf"
	sessionmocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

func TestExecuteTaskOutcomes(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		var calls atomic.Int32
		err := executeTask(context.Background(), nil, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("accepted", func(t *testing.T) {
		type contextKey struct{}
		ctx := context.WithValue(context.Background(), contextKey{}, "executor")
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
			return task(ctx)
		})
		err := executeTask(ctx, executor, func(taskCtx context.Context) error {
			calls.Add(1)
			assert.Equal(t, "executor", taskCtx.Value(contextKey{}))
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("rejected", func(t *testing.T) {
		expected := errors.New("submission rejected")
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(context.Context, component.Task) error {
			return expected
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, expected)
		assert.Zero(t, calls.Load())
	})

	t.Run("skipped", func(t *testing.T) {
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(context.Context, component.Task) error {
			return component.ErrTaskSkipped
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, component.ErrTaskSkipped)
		assert.Zero(t, calls.Load())
	})

	t.Run("skip_joined_with_rejection", func(t *testing.T) {
		rejected := errors.New("submission rejected")
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(context.Context, component.Task) error {
			return errors.Join(component.ErrTaskSkipped, rejected)
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, component.ErrTaskSkipped)
		assert.ErrorIs(t, err, rejected)
		assert.False(t, isUninvokedTaskSkip(err))
		assert.Zero(t, calls.Load())
	})

	t.Run("nil_without_task", func(t *testing.T) {
		executor := component.TaskExecutorFunc(func(context.Context, component.Task) error {
			return nil
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			return nil
		})
		assert.ErrorIs(t, err, errTaskExecutorDidNotComplete)
	})

	t.Run("executor_panic", func(t *testing.T) {
		executor := component.TaskExecutorFunc(func(context.Context, component.Task) error {
			panic("executor panic")
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			return nil
		})
		assert.ErrorContains(t, err, "task executor panic: executor panic")
	})

	t.Run("task_panic", func(t *testing.T) {
		executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
			return task(ctx)
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			panic("task panic")
		})
		assert.ErrorIs(t, err, component.ErrTaskPanic)
		assert.ErrorContains(t, err, "task panic")
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(ctx context.Context, _ component.Task) error {
			return ctx.Err()
		})
		err := executeTask(ctx, executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, calls.Load())
	})

	t.Run("error_after_task", func(t *testing.T) {
		expected := errors.New("executor failed after task")
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
			require.NoError(t, task(ctx))
			return expected
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, errTaskExecutorFailedAfterCompletion)
		assert.ErrorIs(t, err, expected)
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("skip_after_task", func(t *testing.T) {
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
			require.NoError(t, task(ctx))
			return component.ErrTaskSkipped
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, errTaskExecutorFailedAfterCompletion)
		assert.ErrorIs(t, err, component.ErrTaskSkipped)
		assert.False(t, isUninvokedTaskSkip(err))
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("duplicate", func(t *testing.T) {
		var calls atomic.Int32
		executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
			require.NoError(t, task(ctx))
			return task(ctx)
		})
		err := executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		assert.ErrorIs(t, err, errTaskExecutorInvokedMoreThanOnce)
		assert.ErrorIs(t, err, component.ErrTaskAlreadyInvoked)
		assert.Equal(t, int32(1), calls.Load())
	})
}

func TestExecuteTaskWaitsForEarlyInvocation(t *testing.T) {
	taskStarted := make(chan struct{})
	executorReturning := make(chan struct{})
	releaseTask := make(chan struct{})
	result := make(chan error, 1)
	var submittedTask component.Task
	var calls atomic.Int32

	executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
		submittedTask = task
		go func() {
			_ = task(ctx)
		}()
		<-taskStarted
		close(executorReturning)
		return component.ErrTaskSkipped
	})

	go func() {
		result <- executeTask(context.Background(), executor, func(context.Context) error {
			calls.Add(1)
			close(taskStarted)
			<-releaseTask
			return nil
		})
	}()

	<-executorReturning
	require.Eventually(t, func() bool {
		return errors.Is(submittedTask(context.Background()), component.ErrTaskInvocationClosed)
	}, time.Second, time.Millisecond)
	select {
	case err := <-result:
		t.Fatalf("executeTask returned while task was running: %v", err)
	default:
	}
	close(releaseTask)
	err := <-result
	assert.ErrorIs(t, err, errTaskExecutorReturnedEarly)
	assert.ErrorIs(t, err, component.ErrTaskSkipped)
	assert.False(t, isUninvokedTaskSkip(err))
	assert.Equal(t, int32(1), calls.Load())
}

func TestExecuteTaskRejectsLateInvocation(t *testing.T) {
	var lateTask component.Task
	var calls atomic.Int32
	executor := component.TaskExecutorFunc(func(_ context.Context, task component.Task) error {
		lateTask = task
		return component.ErrTaskSkipped
	})

	err := executeTask(context.Background(), executor, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	assert.ErrorIs(t, err, component.ErrTaskSkipped)
	require.NotNil(t, lateTask)
	assert.ErrorIs(t, lateTask(context.Background()), component.ErrTaskInvocationClosed)
	assert.Zero(t, calls.Load())
}

func TestExecuteTaskRejectsLateInvocationAfterCompletion(t *testing.T) {
	var lateTask component.Task
	var calls atomic.Int32
	executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
		lateTask = task
		return task(ctx)
	})

	err := executeTask(context.Background(), executor, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, lateTask)
	assert.ErrorIs(t, lateTask(context.Background()), component.ErrTaskInvocationClosed)
	assert.Equal(t, int32(1), calls.Load())
}

func TestExecuteTaskAllowsOneConcurrentInvocation(t *testing.T) {
	const attempts = 8
	var calls atomic.Int32
	executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
		start := make(chan struct{})
		errs := make(chan error, attempts)
		for range attempts {
			go func() {
				<-start
				errs <- task(ctx)
			}()
		}
		close(start)
		joined := make([]error, 0, attempts)
		for range attempts {
			joined = append(joined, <-errs)
		}
		return errors.Join(joined...)
	})

	err := executeTask(context.Background(), executor, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	assert.ErrorIs(t, err, component.ErrTaskAlreadyInvoked)
	assert.Equal(t, int32(1), calls.Load())
}

type TaskDispatchComponent struct {
	component.Base
	calls *atomic.Int32
}

func (c *TaskDispatchComponent) Invoke(context.Context, *test.SomeStruct) (*test.SomeStruct, error) {
	c.calls.Add(1)
	return &test.SomeStruct{B: "executed"}, nil
}

func TestTaskExecutorHandlerRemoteParity(t *testing.T) {
	rejected := errors.New("submission rejected")
	publicRejected := apierrors.ServiceUnavailable("queue_full", "queue unavailable", "")
	tests := []struct {
		name       string
		msgType    message.Type
		executor   component.TaskExecutor
		wantCalls  int32
		wantAfter  int32
		wantAck    bool
		wantErr    error
		wantStatus string
		forbidText string
	}{
		{
			name:      "request_accepted",
			msgType:   message.Request,
			executor:  component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error { return task(ctx) }),
			wantCalls: 1,
			wantAfter: 1,
		},
		{
			name:       "request_skipped",
			msgType:    message.Request,
			executor:   component.TaskExecutorFunc(func(context.Context, component.Task) error { return component.ErrTaskSkipped }),
			wantErr:    component.ErrTaskSkipped,
			wantStatus: "service_unavailable",
		},
		{
			name:     "notify_skipped",
			msgType:  message.Notify,
			executor: component.TaskExecutorFunc(func(context.Context, component.Task) error { return component.ErrTaskSkipped }),
			wantAck:  true,
		},
		{
			name:       "request_rejected",
			msgType:    message.Request,
			executor:   component.TaskExecutorFunc(func(context.Context, component.Task) error { return rejected }),
			wantErr:    rejected,
			wantStatus: "internal",
		},
		{
			name:       "notify_rejected",
			msgType:    message.Notify,
			executor:   component.TaskExecutorFunc(func(context.Context, component.Task) error { return rejected }),
			wantErr:    rejected,
			wantStatus: "internal",
		},
		{
			name:    "notify_skip_joined_with_rejection",
			msgType: message.Notify,
			executor: component.TaskExecutorFunc(func(context.Context, component.Task) error {
				return errors.Join(component.ErrTaskSkipped, rejected)
			}),
			wantErr:    rejected,
			wantStatus: "internal",
		},
		{
			name:    "request_execute_then_skip",
			msgType: message.Request,
			executor: component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
				_ = task(ctx)
				return component.ErrTaskSkipped
			}),
			wantCalls:  1,
			wantErr:    errTaskExecutorFailedAfterCompletion,
			wantStatus: "internal",
		},
		{
			name:    "notify_execute_then_skip",
			msgType: message.Notify,
			executor: component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
				_ = task(ctx)
				return component.ErrTaskSkipped
			}),
			wantCalls:  1,
			wantErr:    errTaskExecutorFailedAfterCompletion,
			wantStatus: "internal",
		},
		{
			name:       "request_public_rejection",
			msgType:    message.Request,
			executor:   component.TaskExecutorFunc(func(context.Context, component.Task) error { return publicRejected }),
			wantErr:    publicRejected,
			wantStatus: "service_unavailable",
		},
		{
			name:    "request_execute_then_public_rejection",
			msgType: message.Request,
			executor: component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
				_ = task(ctx)
				return publicRejected
			}),
			wantCalls:  1,
			wantErr:    errTaskExecutorFailedAfterCompletion,
			wantStatus: "internal",
			forbidText: "queue unavailable",
		},
		{
			name:    "request_executor_panic",
			msgType: message.Request,
			executor: component.TaskExecutorFunc(func(context.Context, component.Task) error {
				panic("sensitive executor detail")
			}),
			wantErr:    component.ErrTaskExecutorPanic,
			wantStatus: "internal",
			forbidText: "sensitive executor detail",
		},
		{
			name:       "request_canceled",
			msgType:    message.Request,
			executor:   component.TaskExecutorFunc(func(context.Context, component.Task) error { return context.Canceled }),
			wantErr:    context.Canceled,
			wantStatus: "client_closed",
		},
		{
			name:       "request_timed_out",
			msgType:    message.Request,
			executor:   component.TaskExecutorFunc(func(context.Context, component.Task) error { return context.DeadlineExceeded }),
			wantErr:    context.DeadlineExceeded,
			wantStatus: "gateway_timeout",
		},
	}

	for _, testCase := range tests {
		for _, dispatch := range []string{"handler", "remote"} {
			t.Run(testCase.name+"_"+dispatch, func(t *testing.T) {
				var calls atomic.Int32
				var afterCalls atomic.Int32
				comp := &TaskDispatchComponent{calls: &calls}
				rt := route.NewRoute("", "taskexecutor", "Invoke")
				data, err := proto.Marshal(&test.SomeStruct{})
				require.NoError(t, err)

				var responseData []byte
				var responseErr error
				if dispatch == "handler" {
					responseData, responseErr = dispatchTaskExecutorHandler(
						t, comp, testCase.executor, rt, data, testCase.msgType, &afterCalls,
					)
				} else {
					responseData, responseErr = dispatchTaskExecutorRemote(
						t, comp, testCase.executor, rt, data, testCase.msgType, &afterCalls,
					)
				}

				assert.Equal(t, testCase.wantCalls, calls.Load())
				assert.Equal(t, testCase.wantAfter, afterCalls.Load())
				if testCase.wantErr != nil {
					wireError := apierrors.FromError(responseErr)
					switch testCase.wantStatus {
					case "service_unavailable":
						assert.True(t, apierrors.IsServiceUnavailable(responseErr))
					case "client_closed":
						assert.True(t, apierrors.IsClientClosed(responseErr))
					case "gateway_timeout":
						assert.True(t, apierrors.IsGatewayTimeout(responseErr))
					default:
						assert.True(t, apierrors.IsInternalServer(responseErr))
						assert.Equal(t, "component task execution failed", wireError.Status.Message)
					}
					if dispatch == "handler" {
						assert.ErrorIs(t, responseErr, testCase.wantErr)
					}
					if testCase.forbidText != "" {
						assert.NotContains(t, wireError.Status.Message, testCase.forbidText)
					}
					assert.Empty(t, responseData)
					return
				}
				require.NoError(t, responseErr)
				if testCase.wantAck {
					assert.Equal(t, []byte("ack"), responseData)
					return
				}
				response := &test.SomeStruct{}
				require.NoError(t, proto.Unmarshal(responseData, response))
				assert.Equal(t, "executed", response.B)
			})
		}
	}
}

func dispatchTaskExecutorHandler(
	t *testing.T,
	comp *TaskDispatchComponent,
	executor component.TaskExecutor,
	rt *route.Route,
	data []byte,
	msgType message.Type,
	afterCalls *atomic.Int32,
) ([]byte, error) {
	t.Helper()
	service := component.NewService(comp, []component.Option{
		component.WithName(rt.Service),
		component.WithTaskExecutor(executor),
	})
	require.NoError(t, service.ExtractHandler())
	handler, ok := service.Handlers[rt.Method]
	require.True(t, ok)

	pool := NewHandlerPool()
	pool.Register(rt.Service, rt.Method, handler)
	hooks := pipeline.NewHandlerHooks()
	hooks.AfterHandler.PushBack(func(
		ctx context.Context,
		rt route.Route,
		in interface{},
		out interface{},
		err error,
	) (interface{}, error) {
		afterCalls.Add(1)
		return out, err
	})

	ctrl := gomock.NewController(t)
	sess := sessionmocks.NewMockSession(ctrl)
	sess.EXPECT().UID().Return("uid").AnyTimes()
	sess.EXPECT().ID().Return(int64(1)).AnyTimes()
	return pool.ProcessHandlerMessage(
		context.Background(), rt, protobufserializer.NewSerializer(), hooks,
		sess, data, msgType, true,
	)
}

func dispatchTaskExecutorRemote(
	t *testing.T,
	comp *TaskDispatchComponent,
	executor component.TaskExecutor,
	rt *route.Route,
	data []byte,
	msgType message.Type,
	afterCalls *atomic.Int32,
) ([]byte, error) {
	t.Helper()
	hooks := pipeline.NewRemoteHooks()
	hooks.AfterHandler.PushBack(func(
		ctx context.Context,
		rt route.Route,
		in interface{},
		out interface{},
		err error,
	) (interface{}, error) {
		afterCalls.Add(1)
		return out, err
	})
	remoteService := NewRemoteService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
		hooks, pipeline.NewHandlerHooks(), NewHandlerPool(),
	)
	require.NoError(t, remoteService.Register(comp, []component.Option{
		component.WithName(rt.Service),
		component.WithTaskExecutor(executor),
	}))

	protoMsgType := protos.MsgType_MsgRequest
	if msgType == message.Notify {
		protoMsgType = protos.MsgType_MsgNotify
	}
	response := remoteService.handleRPCUser(context.Background(), &protos.Request{
		Msg: &protos.Msg{Route: rt.Short(), Type: protoMsgType, Data: data},
	}, rt)
	if response.Status != nil {
		return nil, apierrors.FromStatus(response.Status)
	}
	return response.Data, nil
}

func BenchmarkExecuteTask(b *testing.B) {
	task := component.Task(func(context.Context) error { return nil })
	executor := component.TaskExecutorFunc(func(ctx context.Context, task component.Task) error {
		return task(ctx)
	})

	b.Run("direct", func(b *testing.B) {
		for range b.N {
			_ = executeTask(context.Background(), nil, task)
		}
	})
	b.Run("executor_sync", func(b *testing.B) {
		for range b.N {
			_ = executeTask(context.Background(), executor, task)
		}
	})
}
