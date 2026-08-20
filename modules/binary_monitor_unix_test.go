//go:build linux || darwin

package modules

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestBinaryMonitorProcessesOutputBeforeChildExit(t *testing.T) {
	releasePath := filepath.Join(t.TempDir(), "release.fifo")
	require.NoError(t, syscall.Mkfifo(releasePath, 0o600))
	script := filepath.Join(t.TempDir(), "emit-output.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf 'monitor-line\\n'\nread -r _ < \"$1\"\n"), 0o700))

	monitorStarted := make(chan struct{})
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(io.Discard),
		zapcore.DebugLevel,
	)
	core = zapcore.RegisterHooks(core, func(entry zapcore.Entry) error {
		if entry.Message == "monitor-line" {
			select {
			case <-monitorStarted:
			default:
				close(monitorStarted)
			}
		}
		return nil
	})
	previousLog := logger.Log
	logger.Log = zap.New(core).Sugar()
	t.Cleanup(func() { logger.Log = previousLog })

	binary := NewBinary(script, []string{releasePath})
	require.NoError(t, binary.Init())
	select {
	case <-monitorStarted:
	case <-time.After(time.Second):
		t.Fatal("stdout monitor did not observe child output")
	}

	release, err := os.OpenFile(releasePath, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = release.WriteString("\n")
	require.NoError(t, err)
	require.NoError(t, release.Close())
	select {
	case <-binary.GetExitChannel():
	case <-time.After(time.Second):
		t.Fatal("binary exit was not published after child exit")
	}
}
