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

package metrics

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/logger"

	"github.com/alkaid/pitaya/v2/constants"

	pcontext "github.com/alkaid/pitaya/v2/context"
	"go.uber.org/zap"
)

// ReportTimingFromCtx reports the latency from the context
func ReportTimingFromCtx(ctx context.Context, reporters []Reporter, typ string, err error) {
	if ctx == nil {
		return
	}
	code := apierrors.Code(err)
	status := "ok"
	if err != nil {
		status = "failed"
	}
	if len(reporters) > 0 {
		startTime, ok := pcontext.GetFromPropagateCtx(ctx, constants.StartTimeKey).(int64)
		if !ok {
			return
		}
		route, ok := pcontext.GetFromPropagateCtx(ctx, constants.RouteKey).(string)
		if !ok {
			logger.Zap.Error("route is missing or invalid in context")
			return
		}
		elapsed := time.Since(time.Unix(0, startTime))
		tags := getTags(ctx, map[string]string{
			"route":  route,
			"status": status,
			"type":   typ,
			"code":   strconv.Itoa(code),
		})
		for _, r := range reporters {
			reportSummary(r, ResponseTime, tags, float64(elapsed.Nanoseconds()))
		}
	}
}

func ReportPoolGoDeadlines(ctx context.Context, poolName string, second int, reporters []Reporter) {
	if ctx == nil {
		return
	}
	if len(reporters) > 0 {
		route, ok := pcontext.GetFromPropagateCtx(ctx, constants.RouteKey).(string)
		if !ok {
			route = "nil"
		}
		tags := getTags(ctx, map[string]string{
			"route":  route,
			"second": strconv.Itoa(second),
			"pool":   poolName,
		})
		for _, r := range reporters {
			reportCount(r, PoolGoDeadlines, tags, 1)
		}
	}
}

// ReportMessageProcessDelayFromCtx reports the delay to process the messages
func ReportMessageProcessDelayFromCtx(ctx context.Context, reporters []Reporter, typ string) {
	if ctx != nil && len(reporters) > 0 {
		startTime, ok := pcontext.GetFromPropagateCtx(ctx, constants.StartTimeKey).(int64)
		if !ok {
			return
		}
		route, ok := pcontext.GetFromPropagateCtx(ctx, constants.RouteKey).(string)
		if !ok {
			return
		}
		elapsed := time.Since(time.Unix(0, startTime))
		tags := getTags(ctx, map[string]string{
			"route": route,
			"type":  typ,
		})
		for _, r := range reporters {
			reportSummary(r, ProcessDelay, tags, float64(elapsed.Nanoseconds()))
		}
	}
}

// ReportNumberOfConnectedClients reports the number of connected clients
func ReportNumberOfConnectedClients(reporters []Reporter, number int64) {
	for _, r := range reporters {
		reportGauge(r, ConnectedClients, map[string]string{}, float64(number))
	}
}

// ReportSysMetrics reports sys metrics
func ReportSysMetrics(reporters []Reporter, period time.Duration) {
	ReportSysMetricsContext(context.Background(), reporters, period)
}

// ReportSysMetricsContext reports sys metrics until the context is canceled.
func ReportSysMetricsContext(ctx context.Context, reporters []Reporter, period time.Duration) {
	if ctx == nil || ctx.Err() != nil || len(reporters) == 0 || period <= 0 {
		return
	}

	reportSysMetrics(reporters)

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

			reportSysMetrics(reporters)
			if ctx.Err() != nil {
				return
			}
			timer.Reset(period)
		}
	}
}

func reportSysMetrics(reporters []Reporter) {
	for _, r := range reporters {
		num := runtime.NumGoroutine()
		m := &runtime.MemStats{}
		runtime.ReadMemStats(m)

		reportGauge(r, Goroutines, map[string]string{}, float64(num))
		reportGauge(r, HeapSize, map[string]string{}, float64(m.Alloc))
		reportGauge(r, HeapObjects, map[string]string{}, float64(m.HeapObjects))
	}
}

// ReportExceededRateLimiting reports the number of requests made
// after exceeded rate limiting in a connection
func ReportExceededRateLimiting(reporters []Reporter) {
	for _, r := range reporters {
		reportCount(r, ExceededRateLimiting, map[string]string{}, 1)
	}
}

func reportCount(reporter Reporter, metric string, tags map[string]string, value float64) {
	if reporter == nil {
		logReporterError(metric, reporter, errNilMetricsReporter)
		return
	}
	logReporterError(metric, reporter, reporter.ReportCount(metric, tags, value))
}

func reportGauge(reporter Reporter, metric string, tags map[string]string, value float64) {
	if reporter == nil {
		logReporterError(metric, reporter, errNilMetricsReporter)
		return
	}
	logReporterError(metric, reporter, reporter.ReportGauge(metric, tags, value))
}

func reportSummary(reporter Reporter, metric string, tags map[string]string, value float64) {
	if reporter == nil {
		logReporterError(metric, reporter, errNilMetricsReporter)
		return
	}
	logReporterError(metric, reporter, reporter.ReportSummary(metric, tags, value))
}

func logReporterError(metric string, reporter Reporter, err error) {
	if err == nil {
		return
	}
	logger.Zap.Error(
		"metrics reporter failed",
		zap.String("metric", metric),
		zap.String("reporter", fmt.Sprintf("%T", reporter)),
		zap.Error(err),
	)
}

func tagsFromContext(ctx context.Context) map[string]string {
	val := pcontext.GetFromPropagateCtx(ctx, constants.MetricTagsKey)
	switch tags := val.(type) {
	case map[string]string:
		return tags
	case map[string]any:
		converted := make(map[string]string, len(tags))
		for key, value := range tags {
			stringValue, ok := value.(string)
			if !ok {
				return map[string]string{}
			}
			converted[key] = stringValue
		}
		return converted
	default:
		return map[string]string{}
	}
}

func getTags(ctx context.Context, tags map[string]string) map[string]string {
	merged := make(map[string]string, len(tags))
	for k, v := range tagsFromContext(ctx) {
		merged[k] = v
	}
	// Framework-owned dimensions describe the operation actually observed and
	// must not be forgeable through propagated application tags.
	for k, v := range tags {
		merged[k] = v
	}

	return merged
}
