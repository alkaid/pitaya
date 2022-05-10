package logger

import (
	"fmt"

	"go.uber.org/zap/zapcore"
)

// ZapCore 自定义zap core,主要实现了替换stacktrace为pkg/errors Wrap后的stacktrace
type ZapCore struct {
	zapcore.Core
}

// WrapCore wraps a Core and runs a collection of user-defined callback
// hooks each time a message is logged. Execution of the callbacks is blocking.
//
// This offers users an easy way to register simple callbacks (e.g., metrics
// collection) without implementing the full Core interface.
func WrapCore(core zapcore.Core) zapcore.Core {
	return &ZapCore{
		Core: core,
	}
}

func (h *ZapCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if h.Enabled(ent.Level) {
		return ce.AddCore(ent, h)
	}
	return ce
}

func (h *ZapCore) With(fields []zapcore.Field) zapcore.Core {
	return &ZapCore{
		Core: h.Core.With(fields),
	}
}

func (h *ZapCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	// 如果fields中有error项，则判断是否fmt.Formatter并替换stack
	for _, field := range fields {
		if field.Type == zapcore.ErrorType {
			e := field.Interface.(error)
			basic := e.Error()
			switch field.Interface.(type) {
			case fmt.Formatter:
				verbose := fmt.Sprintf("%+v", e)
				if verbose != basic {
					ent.Stack = verbose
				}
			}
		}
	}
	return h.Core.Write(ent, fields)
}
