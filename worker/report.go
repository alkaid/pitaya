package worker

import (
	"context"
	"strconv"
	"time"

	workers "github.com/alkaid/pitaya/v2/internal/workers"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics"
)

// Report sends periodic of worker reports
func Report(reporters []metrics.Reporter, period time.Duration) {
	ReportContext(context.Background(), reporters, period)
}

// ReportContext sends periodic worker reports until the context is canceled.
func ReportContext(ctx context.Context, reporters []metrics.Reporter, period time.Duration) {
	if ctx.Err() != nil {
		return
	}

	timer := time.NewTimer(period)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
		}

		workerStats := workers.GetStats()
		for _, r := range reporters {
			reportJobsRetry(r, workerStats.Retries)
			reportQueueSizes(r, workerStats.Enqueued)
			reportJobsTotal(r, workerStats.Failed, workerStats.Processed)
		}

		if ctx.Err() != nil {
			return
		}
		timer.Reset(period)
	}
}

func reportJobsRetry(r metrics.Reporter, retries int64) {
	err := r.ReportGauge(metrics.WorkerJobsRetry, map[string]string{}, float64(retries))
	checkReportErr(metrics.WorkerJobsRetry, err)
}

func reportQueueSizes(r metrics.Reporter, queues map[string]string) {
	for queue, size := range queues {
		tags := map[string]string{"queue": queue}
		sizeFlt, err := strconv.ParseFloat(size, 64)
		if err != nil {
			logger.Log.Errorf("queue size is not int: queue=%s size=%s", queue, size)
			continue
		}

		err = r.ReportGauge(metrics.WorkerQueueSize, tags, sizeFlt)
		checkReportErr(metrics.WorkerQueueSize, err)
	}
}

func reportJobsTotal(r metrics.Reporter, failed, processed int) {
	// "failed" and "processed" always grow up,
	// so they work as count but must be reported as gauge
	err := r.ReportGauge(metrics.WorkerJobsTotal, map[string]string{
		"status": "failed",
	}, float64(failed))
	checkReportErr(metrics.WorkerJobsTotal, err)

	err = r.ReportGauge(metrics.WorkerJobsTotal, map[string]string{
		"status": "ok",
	}, float64(processed))
	checkReportErr(metrics.WorkerJobsTotal, err)
}

func checkReportErr(metric string, err error) {
	if err != nil {
		logger.Log.Errorf("failed to report to %s: %q\n", metric, err)
	}
}
