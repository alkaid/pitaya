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

package util

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"reflect"
	"runtime/debug"
	"strconv"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/pkg/errors"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

	"github.com/nats-io/nuid"

	gonanoid "github.com/matoous/go-nanoid"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/serialize/json"
	"github.com/topfreegames/pitaya/v2/serialize/protobuf"
)

func getLoggerFromArgs(args []reflect.Value) *zap.Logger {
	for _, a := range args {
		if !a.IsValid() {
			continue
		}
		if ctx, ok := a.Interface().(context.Context); ok {
			logVal := ctx.Value(constants.LoggerCtxKey)
			if logVal != nil {
				log := logVal.(*zap.Logger)
				return log
			}
		}
	}
	return logger.Zap
}

func GetLoggerFromCtx(ctx context.Context) *zap.Logger {
	l, ok := ctx.Value(constants.LoggerCtxKey).(*zap.Logger)
	if !ok {
		l = logger.Zap
	}
	return l
}

// SafeCall 安全调用方法,会 recover() 并打印 runtime error
//
// @param onRecovered panic恢复后的回调
// @param method 要调用的方法
func SafeCall(onRecovered func(rec any), method func()) {
	defer func() {
		if rec := recover(); rec != nil {
			// Try to use logger from context here to help trace error cause
			stackTrace := debug.Stack()
			stackTraceAsRawStringLiteral := strconv.Quote(string(stackTrace))
			logger.Zap.DPanic("panic - pitaya method", zap.Any("panicData", rec), zap.String("stack", stackTraceAsRawStringLiteral))
			if onRecovered == nil {
				return
			}
			onRecovered(rec)
		}
	}()
	method()
}

// Pcall calls a method that returns an interface and an error and recovers in case of panic
func Pcall(method reflect.Method, args []reflect.Value) (rets interface{}, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			// Try to use logger from context here to help trace error cause
			stackTrace := debug.Stack()
			stackTraceAsRawStringLiteral := strconv.Quote(string(stackTrace))
			log := getLoggerFromArgs(args)
			log.DPanic("panic - pitaya/dispatch", zap.String("methodName", method.Name), zap.Any("panicData", rec), zap.String("stack", stackTraceAsRawStringLiteral))

			if s, ok := rec.(string); ok {
				err = errors.New(s)
			} else {
				err = fmt.Errorf("rpc call internal error - %s: %v", method.Name, rec)
			}
		}
	}()

	r := method.Func.Call(args)
	// r can have 0 length in case of notify handlers
	// otherwise it will have 2 outputs: an interface and an error
	if len(r) == 2 {
		if v := r[1].Interface(); v != nil {
			err = v.(error)
		} else if !r[0].IsNil() {
			rets = r[0].Interface()
		} else {
			err = constants.ErrReplyShouldBeNotNull
		}
	}
	return
}

// SliceContainsString returns true if a slice contains the string
func SliceContainsString(slice []string, str string) bool {
	for _, value := range slice {
		if value == str {
			return true
		}
	}
	return false
}

// SerializeOrRaw serializes the interface if its not an array of bytes already
func SerializeOrRaw(serializer serialize.Serializer, v interface{}) ([]byte, error) {
	if data, ok := v.([]byte); ok {
		return data, nil
	}
	data, err := serializer.Marshal(v)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return data, nil
}

// FileExists tells if a file exists
func FileExists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil || os.IsExist(err)
}

// GetErrorFromPayload gets the error from payload
func GetErrorFromPayload(serializer serialize.Serializer, payload []byte) error {
	err := apierrors.InternalServer("unknown", "unknown", "")
	switch serializer.(type) {
	case *json.Serializer:
		_ = serializer.Unmarshal(payload, err)
	case *protobuf.Serializer:
		pErr := &apierrors.Status{}
		_ = serializer.Unmarshal(payload, pErr)
		err = apierrors.FromStatusWithoutStack(pErr)
	}
	return err
}

var customErrorToProto ErrorToProto                      // 自定义error转proto
type ErrorToProto func(err error) (proto.Message, error) // 自定义error转proto

// SetErrorToProtoConvertor 设置自定义error转byte函数
//
//	@param errorToProto
func SetErrorToProtoConvertor(errorToProto ErrorToProto) {
	customErrorToProto = errorToProto
}

// GetErrorPayload creates and serializes an error payload
func GetErrorPayload(serializer serialize.Serializer, err error) ([]byte, error) {
	if customErrorToProto != nil {
		msg, err2 := customErrorToProto(err)
		if err2 == nil {
			return SerializeOrRaw(serializer, msg)
		}
		logger.Zap.Error("convert api error to proto exception", zap.Error(err))
	}
	errPayload := &apierrors.FromError(err).Status
	return SerializeOrRaw(serializer, errPayload)
}

// ConvertProtoToMessageType converts a protos.MsgType to a message.Type
func ConvertProtoToMessageType(protoMsgType protos.MsgType) message.Type {
	var msgType message.Type
	switch protoMsgType {
	case protos.MsgType_MsgRequest:
		msgType = message.Request
	case protos.MsgType_MsgNotify:
		msgType = message.Notify
	}
	return msgType
}

func LogFieldsFromCtx(ctx context.Context) []zap.Field {
	fields := make([]zap.Field, 0, 3)
	if rId, ok := pcontext.GetFromPropagateCtx(ctx, constants.RequestIDKey).(string); ok && rId != "" {
		fields = append(fields, zap.String("reqId", rId))
	}
	if uId, ok := pcontext.GetFromPropagateCtx(ctx, constants.UserIdCtxKey).(string); ok && uId != "" {
		fields = append(fields, zap.String("uid", uId))
	}
	if route, ok := pcontext.GetFromPropagateCtx(ctx, constants.RouteKey).(string); ok && route != "" {
		fields = append(fields, zap.String("route", route))
	}
	return fields
}

// CtxWithDefaultLogger inserts a default logger on ctx to be used on handlers and remotes.
// If using logrus, userId, route and requestId will be added as fields.
// Otherwise the pitaya logger will be used as it is.
func CtxWithDefaultLogger(ctx context.Context, route, userID string) context.Context {
	requestID := pcontext.GetFromPropagateCtx(ctx, constants.RequestIDKey)
	var rID string
	var ok bool
	if rID, ok = requestID.(string); ok {
		if rID == "" {
			rID = nuid.Next()
		}
	} else {
		rID = nuid.Next()
	}
	if userID != "" {
		ctx = pcontext.AddToPropagateCtx(ctx, constants.UserIdCtxKey, userID)
	}
	defaultLogger := logger.Zap.With(zap.String("route", route), zap.String("reqId", rID), zap.String("uid", userID))

	return context.WithValue(ctx, constants.LoggerCtxKey, defaultLogger)
}

// GetContextFromRequest gets the context from a request
func GetContextFromRequest(req *protos.Request, serverID string, uid string) (context.Context, error) {
	ctx, err := pcontext.Decode(req.GetMetadata())
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.WithStack(constants.ErrNoContextFound)
	}

	requestID := pcontext.GetFromPropagateCtx(ctx, constants.RequestIDKey)
	if rID, ok := requestID.(string); !ok || (ok && rID == "") {
		requestID = nuid.New().Next()
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RequestIDKey, requestID)
	}

	route := req.GetMsg().GetRoute()
	ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, route)
	ctx = CtxWithDefaultLogger(ctx, route, "")
	return ctx, nil
}

func MapStrInter2MapStrStr(in map[string]interface{}) map[string]string {
	result := map[string]string{}
	for k := range in {
		if v, ok := in[k].(string); ok {
			result[k] = v
		}
	}
	for k, v := range in {
		if str, ok := v.(string); ok {
			result[k] = str
		} else {
			logger.Zap.Error("value type is not string")
		}
	}
	return result
}

func ForceIdStrToInt(id string) int64 {
	if id == "" {
		return 0
	}
	idInt, err := strconv.ParseInt(id, 10, 64)
	if err == nil {
		return idInt
	}
	idInt = int64(crc32.ChecksumIEEE([]byte(id)))
	return idInt
}

const defaultNanoIDLen = 16

// NanoID 随机唯一ID like UUID
//
//	@return string
func NanoID(length ...int) string {
	length_ := defaultNanoIDLen
	if len(length) > 0 {
		length_ = length[0]
	}
	id, err := gonanoid.ID(length_)
	if err != nil {
		logger.Log.Error("", zap.Error(err))
	}
	return id
}
