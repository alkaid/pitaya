package service

import (
	"context"
	"fmt"
	"github.com/topfreegames/pitaya/v2/logger"
	"reflect"
	"sync"

	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/topfreegames/pitaya/v2/component"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
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
	handleName := fmt.Sprintf("%s.%s", serviceName, name)
	if _, ok := h.handlers[handleName]; ok {
		logger.Zap.Warn("handler already registered,overwriting", zap.String("name", handleName))
	}
	h.handlers[fmt.Sprintf("%s.%s", serviceName, name)] = handler
}

// RegisterInterceptor 注册拦截分发器,优先级别高于 component.Handler
//
//	@receiver h
//	@param serviceName
//	@param interceptor
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
			return nil, apierrors.FromError(err)
		}
		// l := ctx.Value(constants.LoggerCtxKey).(*zap.Logger)
		// l.Debug("SID=%d, Data=%s", session.ID(), data)
		var resp any
		resp, err = interceptor.InterceptorFun(ctx, *rt, data)
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
		return nil, apierrors.NotFound("", "process handle message error", "").WithCause(err)
	}

	msgType, err := getMsgType(msgTypeIface)
	if err != nil {
		return nil, apierrors.FromError(err)
	}

	l := ctx.Value(constants.LoggerCtxKey).(*zap.Logger)
	exit, err := handler.ValidateMessageType(msgType)
	if err != nil && exit {
		return nil, apierrors.BadRequest("", "process handle message error", "").WithCause(err)
	} else if err != nil {
		l.Warn("invalid message type", zap.Error(err))
	}

	// First unmarshal the handler arg that will be passed to
	// both handler and pipeline functions
	arg, err := unmarshalHandlerArg(handler, serializer, data)
	if err != nil {
		return nil, apierrors.BadRequest("", "process handle message error", "").WithCause(err)
	}

	ctx, arg, err = handlerHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
	if err != nil {
		return nil, err
	}

	l.Debug("process handle message", zap.Int64("SID", session.ID()), zap.ByteString("Data", data))
	receiver := handler.Receiver
	if handler.Options.ReceiverProvider != nil {
		rec := handler.Options.ReceiverProvider(ctx)
		if rec == nil {
			l.Warn("pitaya/handle: route not found,the ReceiverProvider return nil", zap.String("route", rt.Short()))
			return nil, apierrors.NotFound("", "process handle message error", "").WithCause(err)
		}
		receiver = reflect.ValueOf(rec)
	}
	args := []reflect.Value{receiver, reflect.ValueOf(ctx)}
	if arg != nil {
		args = append(args, reflect.ValueOf(arg))
	}
	var resp any
	if handler.Options.TaskGoProvider != nil {
		// 若提供了自定义派发线程
		var wg sync.WaitGroup
		wg.Add(1)
		final := func() { wg.Done() }
		handler.Options.TaskGoProvider(ctx, final, func(ctx context.Context) {
			defer final()
			args[1] = reflect.ValueOf(ctx)
			resp, err = util.Pcall(handler.Method, args)
		})
		wg.Wait()
	} else {
		// 默认在rpc server提供的线程里
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

	resp, err = handlerHooks.AfterHandler.ExecuteAfterPipeline(ctx, rt, arg, resp, err)
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
