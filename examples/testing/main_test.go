package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
