// Package zap
// Deprecated 适配旧的日志打印,请勿使用
package logger

import (
	"github.com/topfreegames/pitaya/v2/logger/interfaces"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var _level = zap.NewAtomicLevelAt(zapcore.InfoLevel)

type oldAdapter struct {
	log *Logger
}

// New create a new oldAdapter (not support log rotating).
func newAdapter(log *Logger) interfaces.Logger {
	return &oldAdapter{log: log}
}

func (l *oldAdapter) Fatal(format ...interface{}) {
	l.log.Sugar.Fatal(format...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Fatalf(format string, args ...interface{}) {
	l.log.Sugar.Fatalf(format, args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Fatalln(format ...interface{}) {
	l.Fatal(format...)
}

func (l *oldAdapter) Debug(args ...interface{}) {
	l.log.Sugar.Debug(args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Debugf(format string, args ...interface{}) {
	l.log.Sugar.Debugf(format, args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Debugln(format ...interface{}) {
	l.Debug(format...)
}

func (l *oldAdapter) Error(args ...interface{}) {
	l.log.Sugar.Error(args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Errorf(format string, args ...interface{}) {
	l.log.Sugar.Errorf(format, args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Errorln(format ...interface{}) {
	l.Error(format...)
}

func (l *oldAdapter) Info(args ...interface{}) {
	l.log.Sugar.Info(args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Infof(format string, args ...interface{}) {
	l.log.Sugar.Infof(format, args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Infoln(format ...interface{}) {
	l.Info(format...)
}

func (l *oldAdapter) Warn(args ...interface{}) {
	l.log.Sugar.Warn(args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Warnf(format string, args ...interface{}) {
	l.log.Sugar.Warnf(format, args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Warnln(format ...interface{}) {
	l.Warn(format...)
}

func (l *oldAdapter) Panic(args ...interface{}) {
	l.log.Sugar.Panic(args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Panicf(format string, args ...interface{}) {
	l.log.Sugar.Panicf(format, args...)
	//l.log.Sugar.Sync()
}
func (l *oldAdapter) Panicln(format ...interface{}) {
	l.Panic(format...)
}

func (l *oldAdapter) WithFields(fields map[string]interface{}) interfaces.Logger {
	args := []interface{}{}
	for k, v := range fields {
		args = append(args, k)
		args = append(args, v)
	}
	s := l.log.Log.Sugar().With(args...)
	return &oldAdapter{log: &Logger{
		Log:   l.log.Log,
		Sugar: s,
		Level: l.log.Level,
	}}
}
func (l *oldAdapter) WithField(key string, value interface{}) interfaces.Logger {
	args := []interface{}{}
	args = append(args, key)
	args = append(args, value)
	s := l.log.Log.Sugar().With(args...)
	return &oldAdapter{log: &Logger{
		Log:   l.log.Log,
		Sugar: s,
		Level: l.log.Level,
	}}
}
func (l *oldAdapter) WithError(err error) interfaces.Logger {
	return l
}
