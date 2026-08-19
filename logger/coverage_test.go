package logger

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type formatterTestError struct{}

func (formatterTestError) Error() string { return "plain formatter error" }

func (formatterTestError) Format(state fmt.State, verb rune) {
	if verb == 'v' && state.Flag('+') {
		_, _ = fmt.Fprint(state, "verbose formatter stack")
		return
	}
	_, _ = fmt.Fprint(state, "plain formatter error")
}

func newObservedTestLogger() (*Logger, *observer.ObservedLogs) {
	core, observed := observer.New(zapcore.DebugLevel)
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	state := newReloadableCoreState(core, cfg)
	log := zap.New(&reloadableCore{state: state})
	return &Logger{
		Log:     log,
		Sugar:   log.Sugar(),
		Level:   cfg.Level,
		options: &Options{},
		core:    state,
	}, observed
}

func TestZapCoreFiltersWritesAndPreservesFormatterStack(t *testing.T) {
	core, observed := observer.New(zapcore.InfoLevel)
	log := zap.New(WrapCore(core))

	log.Debug("disabled")
	log.With(zap.String("component", "test")).Info(
		"formatted",
		zap.Error(formatterTestError{}),
	)
	log.Info("plain", zap.Error(errors.New("plain error")))

	entries := observed.All()
	require.Len(t, entries, 2)
	assert.Equal(t, "verbose formatter stack", entries[0].Stack)
	assert.Equal(t, "test", entries[0].ContextMap()["component"])
	assert.Empty(t, entries[1].Stack)
}

func TestResetGlobalVarRestoresPublishedFacades(t *testing.T) {
	previousZap, previousSugar, previousLog := Zap, Sugar, Log
	t.Cleanup(func() {
		Zap, Sugar, Log = previousZap, previousSugar, previousLog
	})

	Zap = zap.NewNop()
	Sugar = zap.NewNop().Sugar()
	Log = zap.NewNop().Sugar()
	ResetGlobalVar()

	assert.Same(t, Manager.Log, Zap)
	assert.Same(t, Manager.Sugar, Sugar)
	assert.Same(t, Manager.Sugar, Log)
}

func TestLoggerSetLevelSupportsDocumentedInputTypes(t *testing.T) {
	atomicLevel := zap.NewAtomicLevelAt(zapcore.WarnLevel)
	tests := []struct {
		name  string
		value any
		want  zapcore.Level
	}{
		{name: "atomic level", value: &atomicLevel, want: zapcore.WarnLevel},
		{name: "zapcore level", value: zapcore.DebugLevel, want: zapcore.DebugLevel},
		{name: "int", value: int(zapcore.PanicLevel), want: zapcore.PanicLevel},
		{name: "int8", value: int8(zapcore.FatalLevel), want: zapcore.FatalLevel},
		{name: "string", value: "INFO", want: zapcore.InfoLevel},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log, _ := newObservedTestLogger()
			log.SetLevel(test.value)
			assert.Equal(t, test.want, log.Level.Level())
		})
	}
}

func TestLoggerSetLevelRejectsInvalidInputWithoutChangingLevel(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "int below range", value: int(zapcore.DebugLevel) - 1},
		{name: "int8 above range", value: int8(zapcore.FatalLevel) + 1},
		{name: "invalid string", value: "verbose"},
		{name: "unsupported type", value: struct{}{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log, observed := newObservedTestLogger()
			log.SetLevel(test.value)
			assert.Equal(t, zapcore.InfoLevel, log.Level.Level())
			assert.Equal(t, 1, observed.Len(), "invalid input should be observable")
		})
	}
}

func TestReloadableCoreForwardsWriteAndSync(t *testing.T) {
	underlying, observed := observer.New(zapcore.DebugLevel)
	cfg := zap.NewProductionConfig()
	core := &reloadableCore{state: newReloadableCoreState(underlying, cfg)}

	require.NoError(t, core.Write(
		zapcore.Entry{Level: zapcore.InfoLevel, Message: "direct write"},
		[]zapcore.Field{zap.String("component", "test")},
	))
	require.NoError(t, core.Sync())
	require.Equal(t, 1, observed.Len())
	assert.Equal(t, "test", observed.All()[0].ContextMap()["component"])
}

func TestNewLoggerRejectsInvalidEncoding(t *testing.T) {
	cfg := zap.NewProductionConfig()
	cfg.Encoding = "invalid-encoding"
	assert.Nil(t, NewLogger(cfg))
}

func TestLoaderFactoryProvidesConfigAndRunsCallbacks(t *testing.T) {
	log, _ := newObservedTestLogger()
	callbacks := make([]string, 0, 2)
	loader := log.ReloadFactory(
		"pitaya.log",
		func() { callbacks = append(callbacks, "first") },
		func() { callbacks = append(callbacks, "second") },
	)

	key, value := loader.Provide()
	assert.Equal(t, "pitaya.log", key)
	assert.IsType(t, &LogConf{}, value)

	loader.Reload(key, &LogConf{Development: false, Level: "WARN"})
	assert.Equal(t, zapcore.WarnLevel, log.Level.Level())
	assert.Equal(t, []string{"first", "second"}, callbacks)
}
