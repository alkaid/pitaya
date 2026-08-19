package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/metrics"
	"github.com/alkaid/pitaya/v2/metrics/mocks"
	"go.uber.org/mock/gomock"
)

type reportLifecycleContext struct {
	context.Context
	doneObserved chan struct{}
	once         sync.Once
}

func (c *reportLifecycleContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.doneObserved) })
	return c.Context.Done()
}

type reportLifecycleReporter struct {
	calls atomic.Int64
}

func (r *reportLifecycleReporter) ReportCount(string, map[string]string, float64) error {
	return nil
}

func (r *reportLifecycleReporter) ReportSummary(string, map[string]string, float64) error {
	return nil
}

func (r *reportLifecycleReporter) ReportHistogram(string, map[string]string, float64) error {
	return nil
}

func (r *reportLifecycleReporter) ReportGauge(string, map[string]string, float64) error {
	r.calls.Add(1)
	return nil
}

func TestReportContextCancellation(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		reporter := &reportLifecycleReporter{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			ReportContext(ctx, []metrics.Reporter{reporter}, time.Hour)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("ReportContext did not return for an already canceled context")
		}
		if got := reporter.calls.Load(); got != 0 {
			t.Fatalf("reported %d gauges for an already canceled context", got)
		}
	})

	t.Run("after start", func(t *testing.T) {
		baseCtx, cancel := context.WithCancel(context.Background())
		ctx := &reportLifecycleContext{
			Context:      baseCtx,
			doneObserved: make(chan struct{}),
		}
		reporter := &reportLifecycleReporter{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			ReportContext(ctx, []metrics.Reporter{reporter}, time.Hour)
		}()

		select {
		case <-ctx.doneObserved:
		case <-time.After(time.Second):
			t.Fatal("ReportContext did not start waiting for the first reporting period")
		}
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("ReportContext did not return after cancellation")
		}
		if got := reporter.calls.Load(); got != 0 {
			t.Fatalf("reported %d gauges before the first reporting period", got)
		}
	})
}

func TestReportJobsRetry(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockReporter := mocks.NewMockReporter(ctrl)
		mockReporter.EXPECT().ReportGauge(
			metrics.WorkerJobsRetry,
			map[string]string{},
			float64(10))

		reportJobsRetry(mockReporter, 10)
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockReporter := mocks.NewMockReporter(ctrl)
		mockReporter.EXPECT().ReportGauge(
			metrics.WorkerJobsRetry,
			map[string]string{},
			float64(10)).Return(errors.New("err"))

		reportJobsRetry(mockReporter, 10)
	})
}

func TestReportQueueSizes(t *testing.T) {
	t.Parallel()

	var (
		queue1 = "queuename1"
		queue2 = "queuename2"
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReporter := mocks.NewMockReporter(ctrl)
	mockReporter.EXPECT().ReportGauge(
		metrics.WorkerQueueSize,
		map[string]string{"queue": queue1},
		float64(10))
	mockReporter.EXPECT().ReportGauge(
		metrics.WorkerQueueSize,
		map[string]string{"queue": queue2},
		float64(20))

	reportQueueSizes(mockReporter, map[string]string{
		queue1: "10",
		queue2: "20",
	})
}

func TestReportJobsTotal(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockReporter := mocks.NewMockReporter(ctrl)
	mockReporter.EXPECT().ReportGauge(
		metrics.WorkerJobsTotal,
		map[string]string{"status": "failed"},
		float64(10))

	mockReporter.EXPECT().ReportGauge(
		metrics.WorkerJobsTotal,
		map[string]string{"status": "ok"},
		float64(20))

	reportJobsTotal(mockReporter, 10, 20)
}
