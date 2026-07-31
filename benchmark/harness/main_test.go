package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := writeAtomic(path, "127.0.0.1:43210"); err != nil {
		t.Fatalf("writeAtomic() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(contents), "127.0.0.1:43210\n"; got != want {
		t.Fatalf("ready contents = %q, want %q", got, want)
	}
}

func TestWriteAtomicFailsWithoutParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "ready")
	if err := writeAtomic(path, "127.0.0.1:43210"); err == nil {
		t.Fatal("writeAtomic() unexpectedly succeeded without a parent directory")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() error = %v, want file to remain unpublished", err)
	}
}

func TestRunHonorsCancellationBeforeServerStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	readyFile := filepath.Join(root, "ready")
	diagnosticsDir := filepath.Join(root, "diagnostics")

	if err := run(ctx, readyFile, diagnosticsDir, "pitaya/benchmark/canceled/"); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if _, err := os.Stat(readyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ready file Stat() error = %v, want unpublished readiness", err)
	}
	for _, name := range []string{"frontend.log", "backend.log"} {
		if _, err := os.Stat(filepath.Join(diagnosticsDir, name)); err != nil {
			t.Fatalf("diagnostic %s was not written: %v", name, err)
		}
	}
}
