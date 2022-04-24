package service

import (
	"context"
	"fmt"
	"reflect"

	"github.com/topfreegames/pitaya/v2/co"
	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	e "github.com/topfreegames/pitaya/v2/errors"
	"github.com/topfreegames/pitaya/v2/logger/interfaces"
	"github.com/topfreegames/pitaya/v2/pipeline"
	"github.com/topfreegames/pitaya/v2/route"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/util"
)

// HandlerPool ...
type HandlerPool struct {
	handlers     map[string]*component.Handler     // all handler method
	interceptors map[string]*component.Interceptor // 所有拦截分发器,优先级别高于 handlers
}

// NewHandlerPool ...
func NewHandlerPool() *HandlerPool {
	return &HandlerPool{
		handlers:     make(map[string]*component.Handler),
		interceptors: make(map[string]*component.Interceptor),
	}
}

// Register ...
func (h *HandlerPool) Register(serviceName string, name string, handler *component.Handler) {
	h.handlers[fmt.Sprintf("%s.%s", serviceName, name)] = handler
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Handler
//  @receiver h
//  @param serviceName
//  @param interceptor
func (h *HandlerPool) RegisterInterceptor(serviceName string, interceptor *component.Interceptor) {
	h.interceptors[serviceName] = interceptor
}

// GetHandlers ...
func (h *HandlerPool) GetHandlers() map[string]*component.Handler {
	return h.handlers
}

// ProcessHandlerMessage ...
func (h *HandlerPool) ProcessHandlerMessage(
	ctx context.Context,
	rt *route.Route,
	serializer serialize.Serializer,
	handlerHooks *pipeline.HandlerHooks,
	session session.Session,
	data []byte,
	msgTypeIface interface{},
	remote bool,
) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, constants.SessionCtxKey, session)
	ctx = util.CtxWithDefaultLogger(ctx, rt.String(), session.UID())

	// 拦截器优先工作
	interceptor, err1 := h.getInterceptor(rt)
	if err1 == nil {
		msgType, err := getMsgType(msgTypeIface)
		if err != nil {
			return nil, e.NewError(err, e.ErrInternalCode)
		}
		logger := ctx.Value(constants.LoggerCtxKey).(interfaces.Logger)
		logger.Debugf("SID=%d, Data=%s", session.ID(), data)
		var resp any
		// 若启用了单线程反应堆模型,则派发到全局Looper单例
		if interceptor.EnableReactor {
			resp, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
				return interceptor.InterceptorFun(ctx, *rt, data)
			}).Wait(ctx)
		} else {
			resp, err = interceptor.InterceptorFun(ctx, *rt, data)
		}
		if err != nil {
			return nil, err
		}
		if remote && msgType == message.Notify {
			// This is a special case and should only happen with nats rpc client
			// because we used nats request we have to answer to it or else a timeout
			// will happen in the caller server and will be returned to the client
			// the reason why we don't just Publish is to keep track of failed rpc requests
			// with timeouts, maybe we can improve this flow
			resp = []byte("ack")
		}
		ret, err := serializeReturn(serializer, resp)
		if err != nil {
			return nil, err
		}
		return ret, nil
	}
	// 无拦截器情况下走常规handle
	handler, err := h.getHandler(rt)
	if err1 != nil && err != nil {
		return nil, e.NewError(err, e.ErrNotFoundCode)
	}

	msgType, err := getMsgType(msgTypeIface)
	if err != nil {
		return nil, e.NewError(err, e.ErrInternalCode)
	}

	logger := ctx.Value(constants.LoggerCtxKey).(interfaces.Logger)
	exit, err := handler.ValidateMessageType(msgType)
	if err != nil && exit {
		return nil, e.NewError(err, e.ErrBadRequestCode)
	} else if err != nil {
		logger.Warnf("invalid message type, error: %s", err.Error())
	}

	// First unmarshal the handler arg that will be passed to
	// both handler and pipeline functions
	arg, err := unmarshalHandlerArg(handler, serializer, data)
	if err != nil {
		return nil, e.NewError(err, e.ErrBadRequestCode)
	}

	if handler.Options.EnableReactor {
		arg, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
			ctx, arg, err = handlerHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
			return arg, err
		}).Wait(ctx)
	} else {
		ctx, arg, err = handlerHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
	}
	if err != nil {
		return nil, err
	}

	logger.Debugf("SID=%d, Data=%s", session.ID(), data)
	receiver := handler.Receiver
	if handler.Options.ReceiverProvider != nil {
		rec := handler.Options.ReceiverProvider(ctx)
		if rec == nil {
			logger.Warnf("pitaya/handle: %s not found,the ReceiverProvider return nil", rt.Short())
			return nil, e.NewError(err, e.ErrNotFoundCode)
		}
		receiver = reflect.ValueOf(rec)
	}
	args := []reflect.Value{receiver, reflect.ValueOf(ctx)}
	if arg != nil {
		args = append(args, reflect.ValueOf(arg))
	}
	var resp any
	// 若启用了单线程反应堆模型,则派发到全局Looper单例
	if handler.Options.EnableReactor {
		resp, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
			return util.Pcall(handler.Method, args)
		}).Wait(ctx)
	} else {
		resp, err = util.Pcall(handler.Method, args)
	}
	if remote && msgType == message.Notify {
		// This is a special case and should only happen with nats rpc client
		// because we used nats request we have to answer to it or else a timeout
		// will happen in the caller server and will be returned to the client
		// the reason why we don't just Publish is to keep track of failed rpc requests
		// with timeouts, maybe we can improve this flow
		resp = []byte("ack")
	}

	if handler.Options.EnableReactor {
		resp, err = co.LooperInstance.Async(ctx, func(ctx context.Context, coroutine co.Coroutine) (any, error) {
			return handlerHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, resp, err)
		}).Wait(ctx)
	} else {
		resp, err = handlerHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, resp, err)
	}
	if err != nil {
		return nil, err
	}

	ret, err := serializeReturn(serializer, resp)
	if err != nil {
		return nil, err
	}

	return ret, nil
}

func (h *HandlerPool) getHandler(rt *route.Route) (*component.Handler, error) {
	handler, ok := h.handlers[rt.Short()]
	if !ok {
		e := fmt.Errorf("pitaya/handler: %s not found", rt.String())
		return nil, e
	}
	return handler, nil

}
func (h *HandlerPool) getInterceptor(rt *route.Route) (*component.Interceptor, error) {
	handler, ok := h.interceptors[rt.Service]
	if !ok {
		e := fmt.Errorf("pitaya/handler/dispatcher: %s not found", rt.String())
		return nil, e
	}
	return handler, nil

}
