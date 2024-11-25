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
	Zap = Manager.Log
	Sugar = Manager.Sugar
	Log = Sugar
}

// Logger 具体Log的持有类
type Logger struct {
	Log     *zap.Logger
	Sugar   *zap.SugaredLogger
	Level   zap.AtomicLevel
	options *Options
}

func NewLogger(cfg zap.Config, opts ...Option) *Logger {
	log, err := cfg.Build()
	if err != nil {
		fmt.Printf("uber/zap build error: %s", err.Error())
		return nil
	}
	options := &Options{}
	for _, opt := range opts {
		opt(options)
	}
	if cfg.Encoding == "console" && options.StackWithFmtFormatter {
		log = log.WithOptions(zap.WrapCore(WrapCore))
	}
	return &Logger{
		Log:     log,
		Sugar:   log.Sugar(),
		Level:   cfg.Level,
		options: options,
	}
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
	log, err := cfg.Build()
	if err != nil {
		l.Sugar.Errorf("uber/zap build error: %w", err)
		return
	}
	if cfg.Encoding == "console" && l.options.StackWithFmtFormatter {
		log = log.WithOptions(zap.WrapCore(WrapCore))
	}
	l.Log = log
	l.Sugar = log.Sugar()
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
