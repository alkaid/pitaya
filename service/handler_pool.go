package service

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"go.uber.org/zap"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/pipeline"
	"github.com/alkaid/pitaya/v2/route"
	"github.com/alkaid/pitaya/v2/serialize"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/util"
)

const reservedHandlerServiceName = "sys"

func isReservedHandlerServiceName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), reservedHandlerServiceName)
}

func handlerNotFoundError(rt *route.Route) error {
	return apierrors.NotFound("", "process handle message error", "").WithCause(
		fmt.Errorf("pitaya/handler: %s not found", rt.String()),
	)
}

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
	admissionCtx := ctx
	ctx = context.WithValue(ctx, constants.SessionCtxKey, session)
	ctx = util.CtxWithDefaultLogger(ctx, rt.String(), session.UID())
	if isReservedHandlerServiceName(rt.Service) {
		return nil, handlerNotFoundError(rt)
	}

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
		if err := validateRPCAdmissionAtInvocation(admissionCtx, ctx); err != nil {
			return nil, err
		}
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
	if err != nil {
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

	if admissionErr := validateRPCAdmissionAtInvocation(admissionCtx, ctx); admissionErr != nil {
		return nil, admissionErr
	}
	ctx, arg, err = handlerHooks.BeforeHandler.ExecuteBeforePipeline(ctx, rt, arg)
	if err != nil {
		return nil, err
	}

	l.Debug("process handle message", zap.Int64("SID", session.ID()), zap.ByteString("Data", data))
	receiver := handler.Receiver
	if handler.Options.ReceiverProvider != nil {
		if admissionErr := validateRPCAdmissionAtInvocation(admissionCtx, ctx); admissionErr != nil {
			return nil, admissionErr
		}
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
	// Handler 的业务返回值继续写入 resp/err；executionErr 只描述 TaskExecutor 的调度与完成协议。
	// executeTask 保证闭包完成后才返回，因此后续读取这些共享变量不会与后台 task 竞态。
	executionErr := executeTask(ctx, handler.Options.TaskExecutor, func(taskCtx context.Context) error {
		if admissionErr := validateRPCAdmissionAtInvocation(admissionCtx, taskCtx); admissionErr != nil {
			return admissionErr
		}
		// 自定义执行器可以为任务派生 context，反射调用必须使用实际执行时的 taskCtx。
		args[1] = reflect.ValueOf(taskCtx)
		resp, err = util.Pcall(handler.Method, args)
		// Handler 业务错误保存在外层 err，由既有 dispatcher 流程序列化；这里的错误只属于执行协议。
		return nil
	})
	if executionErr != nil {
		if IsRPCAdmissionRejected(executionErr) {
			return nil, executionErr
		}
		if acknowledgeSkippedTask(msgTypeIface, executionErr, rt, "handler") {
			if remote {
				resp = []byte("ack")
			}
			return serializeReturn(serializer, resp)
		}
		return nil, normalizeTaskExecutionError(executionErr)
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
