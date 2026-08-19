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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestInitLogger(t *testing.T) {
	InitLogMgr(false, "INFO")
	t.Cleanup(func() {
		InitLogMgr(false, "ERROR")
	})

	assert.Equal(t, zapcore.InfoLevel, Manager.Level.Level())
	assert.Same(t, Manager.Log, Zap)
	assert.Same(t, Manager.Sugar, Sugar)
	assert.NotNil(t, Log)
}

func TestNewLogger(t *testing.T) {
	l := NewLogger(zap.NewDevelopmentConfig())

	assert.NotNil(t, l)
	assert.NotNil(t, l.Log)
	assert.NotNil(t, l.Sugar)
	assert.Equal(t, zapcore.DebugLevel, l.Level.Level())
}

func TestLoggerReloadKeepsPublishedFacadesStable(t *testing.T) {
	log := NewLogger(zap.NewProductionConfig())
	zapFacade := log.Log
	sugarFacade := log.Sugar
	loader := log.ReloadFactory("pitaya.log")

	loader.Reload("pitaya.log", &LogConf{Development: true, Level: "DEBUG"})
	loader.Reload("pitaya.log", &LogConf{Development: false, Level: "ERROR"})

	assert.Same(t, zapFacade, log.Log)
	assert.Same(t, sugarFacade, log.Sugar)
	assert.Equal(t, zapcore.ErrorLevel, log.Level.Level())
}

func TestLoggerReloadUpdatesDevelopmentOptions(t *testing.T) {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapcore.FatalLevel)
	log := NewLogger(cfg)

	assert.NotPanics(t, func() { log.Log.DPanic("production dpanic") })
	assert.False(t, reloadableStacktraceEnabler{state: log.core}.Enabled(zapcore.WarnLevel))
	assert.True(t, reloadableStacktraceEnabler{state: log.core}.Enabled(zapcore.ErrorLevel))

	log.SetDevelopment(true)
	assert.PanicsWithValue(t, "development dpanic", func() { log.Log.DPanic("development dpanic") })
	assert.True(t, reloadableStacktraceEnabler{state: log.core}.Enabled(zapcore.WarnLevel))

	log.SetDevelopment(false)
	assert.NotPanics(t, func() { log.Log.DPanic("production dpanic again") })
	assert.False(t, reloadableStacktraceEnabler{state: log.core}.Enabled(zapcore.WarnLevel))
}

func TestLoggerConcurrentReloadAndWrite(t *testing.T) {
	log := NewLogger(zap.NewProductionConfig())
	loader := log.ReloadFactory("pitaya.log")
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			loader.Reload("pitaya.log", &LogConf{
				Development: i%2 == 0,
				Level:       "ERROR",
			})
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			withFields := log.Log.With(zap.String("component", "reload-test"))
			for range iterations {
				log.Log.Debug("concurrent reload")
				log.Sugar.Debug("concurrent reload")
				withFields.Debug("concurrent reload")
			}
		}()
	}
	wg.Wait()
}

func TestGlobalLoggerConcurrentReloadAndWrite(t *testing.T) {
	InitLogMgr(false, "ERROR")
	t.Cleanup(func() { InitLogMgr(false, "ERROR") })
	zapFacade := Zap
	sugarFacade := Sugar
	loader := Manager.ReloadFactory("pitaya.log", ResetGlobalVar)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			loader.Reload("pitaya.log", &LogConf{Development: i%2 == 0, Level: "ERROR"})
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				Zap.Debug("concurrent global reload")
				Sugar.Debug("concurrent global reload")
			}
		}()
	}
	wg.Wait()

	assert.Same(t, zapFacade, Zap)
	assert.Same(t, sugarFacade, Sugar)
}
