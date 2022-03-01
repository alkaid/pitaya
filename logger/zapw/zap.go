// Package zap
// Deprecated 适配旧的日志打印,请勿使用
package zapw

import (
	"github.com/topfreegames/pitaya/v2/logger/interfaces"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var _level = zap.NewAtomicLevelAt(zapcore.InfoLevel)

type logger struct {
	log   *zap.Logger
	sugar *zap.SugaredLogger
}

// New create a new logger (not support log rotating).
func New(cfg zap.Config, options ...zap.Option) interfaces.Logger {
	_level = cfg.Level
	log, _ := cfg.Build(options...)
	return &logger{log: log, sugar: log.Sugar()}
}
func Default() interfaces.Logger {
	return New(zap.NewProductionConfig())
}

func (l *logger) Fatal(format ...interface{}) {
	l.sugar.Fatal(format...)
	//l.sugar.Sync()
}
func (l *logger) Fatalf(format string, args ...interface{}) {
	l.sugar.Fatalf(format, args...)
	//l.sugar.Sync()
}
func (l *logger) Fatalln(format ...interface{}) {
	l.Fatal(format...)
}

func (l *logger) Debug(args ...interface{}) {
	l.sugar.Debug(args...)
	//l.sugar.Sync()
}
func (l *logger) Debugf(format string, args ...interface{}) {
	l.sugar.Debugf(format, args...)
	//l.sugar.Sync()
}
func (l *logger) Debugln(format ...interface{}) {
	l.Debug(format...)
}

func (l *logger) Error(args ...interface{}) {
	l.sugar.Error(args...)
	//l.sugar.Sync()
}
func (l *logger) Errorf(format string, args ...interface{}) {
	l.sugar.Errorf(format, args...)
	//l.sugar.Sync()
}
func (l *logger) Errorln(format ...interface{}) {
	l.Error(format...)
}

func (l *logger) Info(args ...interface{}) {
	l.sugar.Info(args...)
	//l.sugar.Sync()
}
func (l *logger) Infof(format string, args ...interface{}) {
	l.sugar.Infof(format, args...)
	//l.sugar.Sync()
}
func (l *logger) Infoln(format ...interface{}) {
	l.Info(format...)
}

func (l *logger) Warn(args ...interface{}) {
	l.sugar.Warn(args...)
	//l.sugar.Sync()
}
func (l *logger) Warnf(format string, args ...interface{}) {
	l.sugar.Warnf(format, args...)
	//l.sugar.Sync()
}
func (l *logger) Warnln(format ...interface{}) {
	l.Warn(format...)
}

func (l *logger) Panic(args ...interface{}) {
	l.sugar.Panic(args...)
	//l.sugar.Sync()
}
func (l *logger) Panicf(format string, args ...interface{}) {
	l.sugar.Panicf(format, args...)
	//l.sugar.Sync()
}
func (l *logger) Panicln(format ...interface{}) {
	l.Panic(format...)
}

func (l *logger) WithFields(fields map[string]interface{}) interfaces.Logger {
	args := []interface{}{}
	for k, v := range fields {
		args = append(args, k)
		args = append(args, v)
	}
	return &logger{sugar: l.log.Sugar().With(args...)}
}
func (l *logger) WithField(key string, value interface{}) interfaces.Logger {
	args := []interface{}{}
	args = append(args, key)
	args = append(args, value)
	return &logger{log: l.log, sugar: l.sugar.With(args...)}
}
func (l *logger) WithError(err error) interfaces.Logger {
	return l
}
func (l *logger) SetLevel(level interfaces.Level) {
	lowl := zapcore.InfoLevel
	lowl.Set(string(level))
	_level.SetLevel(lowl)
}
