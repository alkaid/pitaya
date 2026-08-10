package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alkaid/pitaya/v2/session"
)

func TestNotifyStressCloseConsumesTracker(t *testing.T) {
	service := &TestSvc{}
	closeEvent := make(chan session.CloseReason, 1)
	service.stressCloseEvents.Store(int64(42), closeEvent)

	service.notifyStressClose(42, session.CloseReasonKickRebind)

	select {
	case got := <-closeEvent:
		if got != session.CloseReasonKickRebind {
			t.Fatalf("close reason = %v, want %v", got, session.CloseReasonKickRebind)
		}
	default:
		t.Fatal("close reason was not delivered")
	}
	if _, tracked := service.stressCloseEvents.Load(int64(42)); tracked {
		t.Fatal("close tracker remains after delivery")
	}
}

func TestWriteReadyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := writeReadyFile(path, "127.0.0.1:43210"); err != nil {
		t.Fatalf("writeReadyFile() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(contents), "127.0.0.1:43210\n"; got != want {
		t.Fatalf("ready contents = %q, want %q", got, want)
	}
}

func TestWriteReadyFileFailsWithoutParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "ready")
	if err := writeReadyFile(path, "127.0.0.1:43210"); err == nil {
		t.Fatal("writeReadyFile() unexpectedly succeeded without a parent directory")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() error = %v, want file to remain unpublished", err)
	}
}
