// Package log
// 提供全局zap组件
package log

import (
	"fmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var _level = zap.NewAtomicLevelAt(zapcore.InfoLevel)

// Log 全局log组件
var Log = New(zap.NewProductionConfig())

// Sugar 全局zap.SugaredLogger组件
var Sugar *zap.SugaredLogger = Log.Sugar()

//New 新建 zap.Logger.若非定制化需求,请使用全局的 Log
//  @param cfg
//  @param options
//  @return *zap.Logger
func New(cfg zap.Config, options ...zap.Option) *zap.Logger {
	_level = cfg.Level
	log, err := cfg.Build(options...)
	if err != nil {
		fmt.Errorf("uber/zap build error: %w", err)
		return nil
	}
	return log
}

//SetLog 设置自定义全局 Log .
//  @param log 自定义log
//  @param level 必须设置参数log关联的level使得打印级别可以动态改变
func SetLog(log *zap.Logger, level zap.AtomicLevel) {
	Log = log
	_level = level
	Sugar = Log.Sugar()
}

//SetLevel 动态改变打印级别
//  @param level
func SetLevel(level zap.AtomicLevel) {
	_level.SetLevel(level.Level())
}
