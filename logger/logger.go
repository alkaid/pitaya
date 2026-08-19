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

package logger

import (
	"errors"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Manager = NewLogger(zap.NewProductionConfig(), WithStackWithFmtFormatter(true))
var Zap = Manager.Log
var Sugar = Manager.Sugar

// Log
//
//	@Deprecated use Zap instead
var Log = Manager.Sugar

func ResetGlobalVar() {
	// The manager publishes stable logger facades, so normal configuration
	// reloads do not need to replace these package variables. Preserve the
	// historical reset behavior only when a caller explicitly replaced one.
	if Zap != Manager.Log {
		Zap = Manager.Log
	}
	if Sugar != Manager.Sugar {
		Sugar = Manager.Sugar
	}
	if Log != Manager.Sugar {
		Log = Manager.Sugar
	}
}

func InitLogMgr(development bool, level string) {
	Manager.SetDevelopment(development)
	Manager.SetLevel(level)
	ResetGlobalVar()
}

// Logger 具体Log的持有类
type Logger struct {
	Log     *zap.Logger
	Sugar   *zap.SugaredLogger
	Level   zap.AtomicLevel
	options *Options
	core    *reloadableCoreState
}

type loggerCoreSnapshot struct {
	core              zapcore.Core
	development       bool
	stacktraceEnabled bool
	stacktraceLevel   zapcore.Level
}

type reloadableCoreState struct {
	snapshot atomic.Pointer[loggerCoreSnapshot]
}

func newReloadableCoreState(core zapcore.Core, cfg zap.Config) *reloadableCoreState {
	state := &reloadableCoreState{}
	state.Store(core, cfg)
	return state
}

func (s *reloadableCoreState) Store(core zapcore.Core, cfg zap.Config) {
	stacktraceLevel := zapcore.ErrorLevel
	if cfg.Development {
		stacktraceLevel = zapcore.WarnLevel
	}
	s.snapshot.Store(&loggerCoreSnapshot{
		core:              core,
		development:       cfg.Development,
		stacktraceEnabled: !cfg.DisableStacktrace,
		stacktraceLevel:   stacktraceLevel,
	})
}

func (s *reloadableCoreState) Load() *loggerCoreSnapshot {
	return s.snapshot.Load()
}

type reloadableStacktraceEnabler struct {
	state *reloadableCoreState
}

func (e reloadableStacktraceEnabler) Enabled(level zapcore.Level) bool {
	snapshot := e.state.Load()
	return snapshot.stacktraceEnabled && level >= snapshot.stacktraceLevel
}

type reloadablePanicHook struct {
	state *reloadableCoreState
}

func (h reloadablePanicHook) OnWrite(entry *zapcore.CheckedEntry, _ []zapcore.Field) {
	if entry.Level == zapcore.PanicLevel ||
		(entry.Level == zapcore.DPanicLevel && h.state.Load().development) {
		panic(entry.Message)
	}
}

type reloadableCore struct {
	state  *reloadableCoreState
	fields []zapcore.Field
}

func (c *reloadableCore) current() zapcore.Core {
	core := c.state.Load().core
	if len(c.fields) > 0 {
		core = core.With(c.fields)
	}
	return core
}

func (c *reloadableCore) Enabled(level zapcore.Level) bool {
	return c.current().Enabled(level)
}

func (c *reloadableCore) With(fields []zapcore.Field) zapcore.Core {
	combined := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	combined = append(combined, c.fields...)
	combined = append(combined, fields...)
	return &reloadableCore{state: c.state, fields: combined}
}

func (c *reloadableCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return c.current().Check(entry, checked)
}

func (c *reloadableCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	return c.current().Write(entry, fields)
}

func (c *reloadableCore) Sync() error {
	return c.current().Sync()
}

func NewLogger(cfg zap.Config, opts ...Option) *Logger {
	options := &Options{}
	for _, opt := range opts {
		opt(options)
	}
	configuredLog, err := buildConfiguredLogger(cfg, options)
	if err != nil {
		fmt.Printf("uber/zap build error: %s", err.Error())
		return nil
	}
	coreState := newReloadableCoreState(configuredLog.Core(), cfg)
	log := configuredLog.WithOptions(
		zap.WrapCore(func(zapcore.Core) zapcore.Core {
			return &reloadableCore{state: coreState}
		}),
		// These zap options live outside Core. Install dynamic variants once so
		// SetDevelopment can keep the published logger pointers stable without
		// freezing production/development semantics at construction time.
		zap.AddStacktrace(reloadableStacktraceEnabler{state: coreState}),
		zap.Development(),
		zap.WithPanicHook(reloadablePanicHook{state: coreState}),
	)
	return &Logger{
		Log:     log,
		Sugar:   log.Sugar(),
		Level:   cfg.Level,
		options: options,
		core:    coreState,
	}
}

func buildConfiguredLogger(cfg zap.Config, options *Options) (*zap.Logger, error) {
	log, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	if cfg.Encoding == "console" && options.StackWithFmtFormatter {
		log = log.WithOptions(zap.WrapCore(WrapCore))
	}
	return log, nil
}

// SetLevel 动态改变打印级别
//
//	@param level 支持的类型为 int,int8,zapcore.Level,string,zap.AtomicLevel. 建议使用string
//	支持的string为 DEBUG|INFO|WARN|ERROR|DPANIC|PANIC|FATAL
func (l *Logger) SetLevel(level any) {
	var err error
	switch val := level.(type) {
	case *zap.AtomicLevel:
		l.Level.SetLevel(val.Level())
	case zapcore.Level:
		l.Level.SetLevel(val)
	case int:
		lnew := zapcore.Level(val)
		if lnew >= zapcore.DebugLevel && lnew <= zapcore.FatalLevel {
			l.Level.SetLevel(lnew)
		} else {
			err = errors.New("illegal log Level")
		}
	case int8:
		lnew := zapcore.Level(val)
		if lnew >= zapcore.DebugLevel && lnew <= zapcore.FatalLevel {
			l.Level.SetLevel(lnew)
		} else {
			err = errors.New("illegal log Level")
		}
	case string:
		lnew := zapcore.ErrorLevel
		err = lnew.Set(val)
		if err == nil {
			l.Level.SetLevel(lnew)
		}
	default:
		err = errors.New("illegal log Level")
	}
	if err != nil {
		l.Log.Error(err.Error())
	}
}

// SetDevelopment 是否开启开发者模式 true为development mode 否则为production mode
//
//	development mode: zap.NewDevelopmentConfig()模式
//	production mode:zap.NewProductionConfig()模式
//	@receiver l
//	@param enable
func (l *Logger) SetDevelopment(enable bool) {
	var cfg zap.Config
	if enable {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = l.Level
	// skip oldAdapter frame
	log, err := buildConfiguredLogger(cfg, l.options)
	if err != nil {
		l.Sugar.Errorf("uber/zap build error: %w", err)
		return
	}
	l.core.Store(log.Core(), cfg)
}

type LogConf struct {
	Development bool
	Level       string
}

func (l *Logger) ReloadFactory(k string, onReloadeds ...func()) LoaderFactory {
	return LoaderFactory{
		ReloadApply: func(key string, confStruct interface{}) {
			c := confStruct.(*LogConf)
			l.SetDevelopment(c.Development)
			l.SetLevel(c.Level)
			l.Log.Debug("change log conf", zap.String("key", key), zap.Any("conf", c))
			if len(onReloadeds) > 0 {
				for _, reloaded := range onReloadeds {
					reloaded()
				}
			}
		},
		ProvideApply: func() (key string, confStruct interface{}) {
			return k, &LogConf{}
		},
	}
}

// LoaderFactory ConfLoader 工厂,用于生成需要动态包装的 ConfLoader 实现
//
//	@implement ConfLoader
type LoaderFactory struct {
	// ConfLoader.Reload 的包装函数
	ReloadApply func(key string, confStruct interface{})
	// ConfLoader.Provide 的包装函数
	ProvideApply func() (key string, confStruct interface{})
}

func (l LoaderFactory) Reload(key string, confStruct interface{}) {
	l.ReloadApply(key, confStruct)
}
func (l LoaderFactory) Provide() (key string, confStruct interface{}) {
	return l.ProvideApply()
}
