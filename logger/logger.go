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

//logger
//Deprecated: user package log instead
package logger

import (
	"errors"
	"fmt"
	"github.com/topfreegames/pitaya/v2/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Manager = NewLogger(zap.NewProductionConfig())
var Zap = Manager.Log
var Sugar = Manager.Sugar

// Log
//  @Deprecated use Zap instead
var Log = newAdapter(Manager)

type Logger struct {
	Log   *zap.Logger
	Sugar *zap.SugaredLogger
	Level zap.AtomicLevel
}

func NewLogger(cfg zap.Config) *Logger {
	log, err := cfg.Build()
	if err != nil {
		fmt.Errorf("uber/zap build error: %w", err)
		return nil
	}
	return &Logger{
		Log:   log,
		Sugar: log.Sugar(),
		Level: cfg.Level,
	}
}

//
//type Level = string
//
//const (
//	// DebugLevel logs are typically voluminous, and are usually disabled in
//	// production.
//	DebugLevel Level = "DEBUG"
//	// InfoLevel is the default logging priority.
//	InfoLevel Level = "INFO"
//	// WarnLevel logs are more important than Info, but don't need individual
//	// human review.
//	WarnLevel Level = "WARN"
//	// ErrorLevel logs are high-priority. If an application is running smoothly,
//	// it shouldn't generate any error-level logs.
//	ErrorLevel Level = "ERROR"
//	// DPanicLevel logs are particularly important errors. In development the
//	// logger panics after writing the message.
//	DPanicLevel Level = "DPANIC"
//	// PanicLevel logs a message, then panics.
//	PanicLevel Level = "PANIC"
//	// FatalLevel logs a message, then calls os.Exit(1).
//	FatalLevel Level = "FATAL"
//
//	_minLevel = DebugLevel
//	_maxLevel = FatalLevel
//)

//SetLevel 动态改变打印级别
//  @param level 支持的类型为int,zapcore.Level,string,int,zap.AtomicLevel. 建议使用string
//  支持的string为 DEBUG|INFO|WARN|ERROR|DPANIC|PANIC|FATAL
func (l *Logger) SetLevel(level interface{}) {
	var err error
	switch level.(type) {
	case *zap.AtomicLevel:
		l.Level.SetLevel(level.(*zap.AtomicLevel).Level())
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		lnew := level.(zapcore.Level)
		if lnew >= zapcore.DebugLevel && lnew <= zapcore.FatalLevel {
			l.Level.SetLevel(lnew)
		} else {
			err = errors.New("illegal log Level")
		}
	case string:
		lnew := zapcore.ErrorLevel
		err = lnew.Set(level.(string))
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

//SetDevelopment 是否开启开发者模式 true为development mode 否则为production mode
//  development mode: zap.NewDevelopmentConfig()模式
//  production mode:zap.NewProductionConfig()模式
//  @receiver l
//  @param enable
func (l *Logger) SetDevelopment(enable bool) {
	var cfg zap.Config
	if enable {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = l.Level
	log, err := cfg.Build()
	if err != nil {
		l.Sugar.Errorf("uber/zap build error: %w", err)
		return
	}
	l.Log = log
	l.Sugar = log.Sugar()
}

type LogConf struct {
	Development bool
	Level       string
}

func (l *Logger) ReloadFactory(k string) config.LoaderFactory {
	return config.LoaderFactory{
		ReloadApply: func(key string, confStruct interface{}) {
			c := confStruct.(*LogConf)
			l.SetDevelopment(c.Development)
			l.SetLevel(c.Level)
			l.Log.Debug("change log conf", zap.String("key", key), zap.Any("conf", c))
		},
		ProvideApply: func() (key string, confStruct interface{}) {
			return k, &LogConf{}
		},
	}
}
