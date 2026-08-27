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

package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/serialize/mocks"
	session_mocks "github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/proto"
)

func TestGetHandlerExists(t *testing.T) {
	rt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	expected := &component.Handler{}
	handlerPool := NewHandlerPool()
	handlerPool.Register(rt.Service, rt.Method, expected)

	h, err := handlerPool.getHandler(rt)
	assert.NoError(t, err)
	assert.Equal(t, expected, h)
}

func TestGetHandlerDoesntExist(t *testing.T) {
	rt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	handlerPool := NewHandlerPool()
	h, err := handlerPool.getHandler(rt)
	assert.Nil(t, h)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("%s not found", rt.String()))
}

func TestGetHandlersReturnsMapSnapshot(t *testing.T) {
	pool := NewHandlerPool()
	expected := &component.Handler{}
	pool.Register("snapshot", "handle", expected)

	external := pool.GetHandlers()
	delete(external, "snapshot.handle")
	external["injected.handle"] = &component.Handler{}

	actual := pool.GetHandlers()
	assert.Same(t, expected, actual["snapshot.handle"])
	assert.NotContains(t, actual, "injected.handle")
}

func TestHandlerPoolPublishesWholeServiceSnapshot(t *testing.T) {
	pool := NewHandlerPool()
	const registrations = 200
	var published atomic.Int64
	done := make(chan struct{})

	go func() {
		defer close(done)
		for index := range registrations {
			serviceName := fmt.Sprintf("service-%03d", index)
			pool.registerService(serviceName, map[string]*component.Handler{
				"first":  {},
				"second": {},
			})
			published.Store(int64(index + 1))
		}
	}()

	var readers sync.WaitGroup
	readers.Add(4)
	for range 4 {
		go func() {
			defer readers.Done()
			for {
				snapshot := pool.loadRegistry()
				visible := int(published.Load())
				for index := range visible {
					serviceName := fmt.Sprintf("service-%03d", index)
					_, first := snapshot.handlers[serviceName+".first"]
					_, second := snapshot.handlers[serviceName+".second"]
					if first != second {
						t.Errorf("service %s was partially visible", serviceName)
						return
					}
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	readers.Wait()

	assert.Len(t, pool.GetHandlers(), registrations*2)
}

func TestHandlerPoolConcurrentRegistrationAndLookup(t *testing.T) {
	pool := NewHandlerPool()
	rt := route.NewRoute("", "runtime", "handle")
	pool.Register(rt.Service, rt.Method, &component.Handler{})
	requireInterceptor := func() *component.Interceptor {
		return &component.Interceptor{InterceptorFun: func(context.Context, route.Route, []byte) (proto.Message, error) {
			return nil, nil
		}}
	}
	assert.NoError(t, pool.RegisterInterceptor(rt.Service, requireInterceptor()))

	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(5)
	go func() {
		defer workers.Done()
		<-start
		for range 1000 {
			assert.NoError(t, pool.RegisterInterceptor(rt.Service, requireInterceptor()))
			pool.Register(rt.Service, rt.Method, &component.Handler{})
		}
	}()
	for range 4 {
		go func() {
			defer workers.Done()
			<-start
			for range 5000 {
				if _, err := pool.getHandler(rt); err != nil {
					t.Errorf("handler lookup failed during registration: %v", err)
					return
				}
				if _, err := pool.getInterceptor(rt); err != nil {
					t.Errorf("interceptor lookup failed during registration: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
}

func TestHandlerPoolRejectsInvalidInterceptorWithoutChangingSnapshot(t *testing.T) {
	pool := NewHandlerPool()
	valid := &component.Interceptor{InterceptorFun: func(context.Context, route.Route, []byte) (proto.Message, error) {
		return nil, nil
	}}
	assert.NoError(t, pool.RegisterInterceptor("valid", valid))

	tests := []struct {
		name        string
		serviceName string
		interceptor *component.Interceptor
	}{
		{name: "nil interceptor", serviceName: "valid"},
		{name: "nil function", serviceName: "valid", interceptor: &component.Interceptor{}},
		{name: "empty service", interceptor: valid},
		{name: "whitespace service", serviceName: " valid ", interceptor: valid},
		{name: "route-shaped service", serviceName: "valid.method", interceptor: valid},
		{name: "reserved service", serviceName: "sys", interceptor: valid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := pool.loadRegistry()
			assert.Error(t, pool.RegisterInterceptor(test.serviceName, test.interceptor))
			after := pool.loadRegistry()
			assert.Same(t, before, after)
		})
	}
}

func BenchmarkHandlerPoolLookup(b *testing.B) {
	pool := NewHandlerPool()
	rt := route.NewRoute("", "benchmark", "handle")
	pool.Register(rt.Service, rt.Method, &component.Handler{})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = pool.getHandler(rt)
	}
}

func TestProcessHandlerMessage(t *testing.T) {
	tObj := &TestType{}

	handlerPool := NewHandlerPool()

	m, ok := reflect.TypeOf(tObj).MethodByName("HandlerPointerRaw")
	assert.True(t, ok)
	assert.NotNil(t, m)
	rt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	primaryHandler := &component.Handler{Receiver: reflect.ValueOf(tObj), Method: m, Type: m.Type.In(2)}
	handlerPool.Register(rt.Service, rt.Method, primaryHandler)

	m, ok = reflect.TypeOf(tObj).MethodByName("HandlerPointerErr")
	assert.True(t, ok)
	assert.NotNil(t, m)
	rtErr := route.NewRoute("", uuid.New().String(), uuid.New().String())
	handlerPool.Register(rtErr.Service, rtErr.Method, &component.Handler{Receiver: reflect.ValueOf(tObj), Method: m, Type: m.Type.In(2)})

	m, ok = reflect.TypeOf(tObj).MethodByName("HandlerPointerStruct")
	assert.True(t, ok)
	assert.NotNil(t, m)
	rtSt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	handlerPool.Register(rtSt.Service, rtSt.Method, &component.Handler{Receiver: reflect.ValueOf(tObj), Method: m, Type: m.Type.In(2)})

	tables := []struct {
		name         string
		route        *route.Route
		errSerReturn error
		errSerialize error
		outSerialize interface{}
		handlerType  message.Type
		msgType      interface{}
		remote       bool
		out          []byte
		err          error
	}{
		{"invalid_route", route.NewRoute("", "no", "no"), nil, nil, nil, message.Request, nil, false, nil, apierrors.NotFound("", "process handle message error", "").WithCause(errors.New("pitaya/handler: no.no not found"))},
		{"invalid_msg_type", rt, nil, nil, nil, message.Request, nil, false, nil, apierrors.FromError(errInvalidMsg)},
		{"failed_handle_args_unmarshal", rt, nil, errors.New("some error"), &test.SomeStruct{}, message.Request, message.Request, false, nil, apierrors.BadRequest("", "process handle message error", "").WithCause(errors.New("some error"))},
		{"failed_pcall", rtErr, nil, nil, &test.SomeStruct{A: 1, B: "ok"}, message.Request, message.Request, false, nil, errors.New("HandlerPointerErr")},
		{"failed_serialize_return", rtSt, errors.New("ser ret error"), nil, &test.SomeStruct{A: 1, B: "ok"}, message.Request, message.Request, false, []byte("failed"), nil},
		{"ok", rt, nil, nil, &test.SomeStruct{}, message.Request, message.Request, false, []byte("ok"), nil},
		{"notify_on_request", rt, nil, nil, &test.SomeStruct{}, message.Request, message.Notify, false, []byte("ok"), nil},
		{"remote_notify", rt, nil, nil, &test.SomeStruct{}, message.Notify, message.Notify, true, []byte("ack"), nil},
	}

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			primaryHandler.MessageType = table.handlerType
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			ss := session_mocks.NewMockSession(ctrl)
			ss.EXPECT().UID().Return("uid").AnyTimes()
			ss.EXPECT().ID().Return(int64(1)).AnyTimes()
			mockSerializer := mocks.NewMockSerializer(ctrl)
			if table.outSerialize != nil {
				mockSerializer.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).Return(table.errSerialize).Do(
					func(p []byte, arg interface{}) {
						arg = table.outSerialize
					})

				if table.errSerReturn != nil {
					mockSerializer.EXPECT().Marshal(gomock.Any()).Return(table.out, table.errSerReturn)
					mockSerializer.EXPECT().Marshal(gomock.Any()).Return(table.out, nil)
				}
			}
			handlerHooks := pipeline.NewHandlerHooks()
			out, err := handlerPool.ProcessHandlerMessage(nil, table.route, mockSerializer, handlerHooks, ss, nil, table.msgType, table.remote)
			assert.Equal(t, table.out, out)
			assert.Equal(t, table.err, err)
		})
	}
}

func TestProcessHandlerMessageBrokenBeforePipeline(t *testing.T) {
	ctrl := gomock.NewController(t)
	rt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	handlerPool := NewHandlerPool()
	handlerPool.Register(rt.Service, rt.Method, &component.Handler{})
	expected := errors.New("oh noes")
	before := func(ctx context.Context, _ route.Route, in interface{}) (context.Context, interface{}, error) {
		return ctx, nil, expected
	}
	beforeHandler := pipeline.NewChannel()
	beforeHandler.PushFront(before)

	handlerHooks := pipeline.NewHandlerHooks()
	handlerHooks.BeforeHandler = beforeHandler
	ss := session_mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return("uid").AnyTimes()
	ss.EXPECT().ID().Return(int64(1)).AnyTimes()
	out, err := handlerPool.ProcessHandlerMessage(nil, rt, nil, handlerHooks, ss, nil, message.Request, false)
	assert.Nil(t, out)
	assert.Equal(t, expected, err)
}

func TestProcessHandlerMessageBrokenAfterPipeline(t *testing.T) {
	tObj := &TestType{}
	m, ok := reflect.TypeOf(tObj).MethodByName("HandlerPointerRaw")
	assert.True(t, ok)
	assert.NotNil(t, m)
	rt := route.NewRoute("", uuid.New().String(), uuid.New().String())
	handlerPool := NewHandlerPool()
	handlerPool.Register(rt.Service, rt.Method, &component.Handler{Receiver: reflect.ValueOf(tObj), Method: m, Type: m.Type.In(2)})

	after := func(ctx context.Context, _ route.Route, _ interface{}, out interface{}, err error) (interface{}, error) {
		return nil, errors.New("oh noes")
	}
	afterHandler := pipeline.NewAfterChannel()
	afterHandler.PushFront(after)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss := session_mocks.NewMockSession(ctrl)
	ss.EXPECT().UID().Return("uid").AnyTimes()
	ss.EXPECT().ID().Return(int64(1)).AnyTimes()

	mockSerializer := mocks.NewMockSerializer(ctrl)
	mockSerializer.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).Return(nil).Do(
		func(p []byte, arg interface{}) {
			arg = &test.SomeStruct{}
		})

	handlerHooks := pipeline.NewHandlerHooks()
	handlerHooks.AfterHandler = afterHandler
	out, err := handlerPool.ProcessHandlerMessage(nil, rt, mockSerializer, handlerHooks, ss, nil, message.Request, false)
	assert.Nil(t, out)
	assert.Equal(t, errors.New("oh noes"), err)
}

type sysHandlerSideEffect struct {
	calls *int
}

func (h *sysHandlerSideEffect) Handle(context.Context) {
	(*h.calls)++
}

func TestProcessHandlerMessageRejectsReservedSysRouteBeforeDispatch(t *testing.T) {
	for _, serviceName := range []string{"sys", "Sys", "sYs", " sys "} {
		t.Run(serviceName, func(t *testing.T) {
			for _, msgType := range []message.Type{message.Request, message.Notify} {
				t.Run(msgType.String(), func(t *testing.T) {
					for _, dispatch := range []string{"interceptor", "handler"} {
						t.Run(dispatch, func(t *testing.T) {
							ctrl := gomock.NewController(t)
							ss := session_mocks.NewMockSession(ctrl)
							ss.EXPECT().UID().Return("uid").AnyTimes()
							ss.EXPECT().ID().Return(int64(1)).AnyTimes()

							calls := 0
							handlerPool := NewHandlerPool()
							switch dispatch {
							case "interceptor":
								err := handlerPool.RegisterInterceptor(serviceName, &component.Interceptor{
									InterceptorFun: func(context.Context, route.Route, []byte) (proto.Message, error) {
										calls++
										return nil, nil
									},
								})
								assert.Error(t, err)
							case "handler":
								receiver := &sysHandlerSideEffect{calls: &calls}
								method, ok := reflect.TypeOf(receiver).MethodByName("Handle")
								assert.True(t, ok)
								handlerPool.Register(serviceName, "method", &component.Handler{
									Receiver: reflect.ValueOf(receiver),
									Method:   method,
								})
							}

							hooks := pipeline.NewHandlerHooks()
							hooks.BeforeHandler.PushBack(func(ctx context.Context, _ route.Route, in interface{}) (context.Context, interface{}, error) {
								calls++
								return ctx, in, nil
							})

							out, err := handlerPool.ProcessHandlerMessage(
								context.Background(),
								route.NewRoute("", serviceName, "method"),
								nil,
								hooks,
								ss,
								nil,
								msgType,
								false,
							)

							assert.Nil(t, out)
							assert.Error(t, err)
							assert.True(t, apierrors.IsNotFound(err), "reserved routes should be indistinguishable from missing routes")
							assert.Zero(t, calls)
						})
					}
				})
			}
		})
	}
}
