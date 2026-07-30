package co

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/ants/v2"
)

func TestGroupRunsAcceptedTasks(t *testing.T) {
	expectedErr := errors.New("task failed")
	group, ctx := WithContext(context.Background())
	group.SetLimit(1)
	var ran atomic.Int32
	group.Go(func() error {
		ran.Add(1)
		return expectedErr
	})
	group.Go(func() error {
		ran.Add(1)
		return nil
	})

	if err := group.Wait(); !errors.Is(err, expectedErr) {
		t.Fatalf("Wait error = %v, want %v", err, expectedErr)
	}
	if got := ran.Load(); got != 2 {
		t.Fatalf("ran tasks = %d, want 2", got)
	}
	if !errors.Is(context.Cause(ctx), expectedErr) {
		t.Fatalf("context cause = %v, want %v", context.Cause(ctx), expectedErr)
	}
}

func TestGroupReportsSubmissionFailureWithoutWaiting(t *testing.T) {
	ants.Release()
	t.Cleanup(ants.Reboot)

	group, ctx := WithContext(context.Background())
	group.SetLimit(1)
	var ran atomic.Bool
	group.Go(func() error {
		ran.Store(true)
		return nil
	})

	waitResult := make(chan error, 1)
	go func() { waitResult <- group.Wait() }()
	select {
	case err := <-waitResult:
		if !errors.Is(err, ants.ErrPoolClosed) {
			t.Fatalf("Wait error = %v, want %v", err, ants.ErrPoolClosed)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("Wait blocked after group task submission was rejected")
	}
	if ran.Load() {
		t.Fatal("group task ran after submission rejection")
	}
	if !errors.Is(context.Cause(ctx), ants.ErrPoolClosed) {
		t.Fatalf("context cause = %v, want %v", context.Cause(ctx), ants.ErrPoolClosed)
	}

	var tryGroup Group
	tryGroup.SetLimit(1)
	if tryGroup.TryGo(func() error { return nil }) {
		t.Fatal("TryGo reported that a rejected task was started")
	}
	if err := tryGroup.Wait(); !errors.Is(err, ants.ErrPoolClosed) {
		t.Fatalf("TryGo Wait error = %v, want %v", err, ants.ErrPoolClosed)
	}
}
