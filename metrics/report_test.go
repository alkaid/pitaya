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
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/metrics/mocks"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type sysMetricsLifecycleReporter struct {
	calls     atomic.Int64
	firstCall chan struct{}
}

type periodicSysMetricsReporter struct {
	calls  atomic.Int64
	cycles chan struct{}
}

func newPeriodicSysMetricsReporter() *periodicSysMetricsReporter {
	return &periodicSysMetricsReporter{cycles: make(chan struct{}, 4)}
}

func (r *periodicSysMetricsReporter) ReportCount(string, map[string]string, float64) error {
	return nil
}

func (r *periodicSysMetricsReporter) ReportSummary(string, map[string]string, float64) error {
	return nil
}

func (r *periodicSysMetricsReporter) ReportHistogram(string, map[string]string, float64) error {
	return nil
}

func (r *periodicSysMetricsReporter) ReportGauge(string, map[string]string, float64) error {
	if r.calls.Add(1)%3 == 0 {
		r.cycles <- struct{}{}
	}
	return nil
}

func newSysMetricsLifecycleReporter() *sysMetricsLifecycleReporter {
	return &sysMetricsLifecycleReporter{firstCall: make(chan struct{})}
}

func (r *sysMetricsLifecycleReporter) ReportCount(string, map[string]string, float64) error {
	return nil
}

func (r *sysMetricsLifecycleReporter) ReportSummary(string, map[string]string, float64) error {
	return nil
}

func (r *sysMetricsLifecycleReporter) ReportHistogram(string, map[string]string, float64) error {
	return nil
}

func (r *sysMetricsLifecycleReporter) ReportGauge(string, map[string]string, float64) error {
	if r.calls.Add(1) == 1 {
		close(r.firstCall)
	}
	return nil
}

func TestReportSysMetricsContextCancellation(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		reporter := newSysMetricsLifecycleReporter()
		done := make(chan struct{})
		go func() {
			defer close(done)
			ReportSysMetricsContext(ctx, []Reporter{reporter}, time.Hour)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("ReportSysMetricsContext did not return for an already canceled context")
		}
		if got := reporter.calls.Load(); got != 0 {
			t.Fatalf("reported %d gauges for an already canceled context", got)
		}
	})

	t.Run("after start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reporter := newSysMetricsLifecycleReporter()
		done := make(chan struct{})
		go func() {
			defer close(done)
			ReportSysMetricsContext(ctx, []Reporter{reporter}, time.Hour)
		}()

		select {
		case <-reporter.firstCall:
		case <-time.After(time.Second):
			t.Fatal("ReportSysMetricsContext did not report immediately")
		}
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("ReportSysMetricsContext did not return after cancellation")
		}
		if got := reporter.calls.Load(); got != 3 {
			t.Fatalf("reported %d gauges before cancellation, want 3", got)
		}
	})
}

func TestReportSysMetricsContextRejectsInactiveInputs(t *testing.T) {
	reporter := newSysMetricsLifecycleReporter()
	tests := []struct {
		name      string
		ctx       context.Context
		reporters []Reporter
		period    time.Duration
	}{
		{name: "nil context", reporters: []Reporter{reporter}, period: time.Second},
		{name: "no reporters", ctx: context.Background(), period: time.Second},
		{name: "zero period", ctx: context.Background(), reporters: []Reporter{reporter}},
		{name: "negative period", ctx: context.Background(), reporters: []Reporter{reporter}, period: -time.Second},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				ReportSysMetricsContext(test.ctx, test.reporters, test.period)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("ReportSysMetricsContext did not reject inactive input")
			}
		})
	}
	assert.Zero(t, reporter.calls.Load())
}

func TestReportSysMetricsContextRepeatsUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reporter := newPeriodicSysMetricsReporter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ReportSysMetricsContext(ctx, []Reporter{reporter}, time.Millisecond)
	}()

	for range 3 {
		select {
		case <-reporter.cycles:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("system metrics did not complete a reporting cycle")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("system metrics did not stop after cancellation")
	}
	assert.GreaterOrEqual(t, reporter.calls.Load(), int64(9))
}

func TestReportSysMetricsWrapperRejectsEmptyReporterList(t *testing.T) {
	assert.NotPanics(t, func() { ReportSysMetrics(nil, time.Second) })
}

func TestReportTimingFromCtx(t *testing.T) {
	t.Run("test-duration", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockMetricsReporter := mocks.NewMockReporter(ctrl)

		originalTs := time.Now().Add(-200 * time.Millisecond).UnixNano()
		expectedRoute := uuid.New().String()
		expectedType := uuid.New().String()
		expectedErr := errors.New(uuid.New().String())
		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, originalTs)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, expectedRoute)

		mockMetricsReporter.EXPECT().ReportSummary(ResponseTime, gomock.Any(), gomock.Any()).Do(
			func(metric string, tags map[string]string, duration float64) {
				assert.InDelta(t, duration, time.Now().UnixNano()-originalTs, 10e6)
			},
		)

		ReportTimingFromCtx(ctx, []Reporter{mockMetricsReporter}, expectedType, expectedErr)
	})

	t.Run("test-tags", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockMetricsReporter := mocks.NewMockReporter(ctrl)

		originalTs := time.Now().UnixNano()
		expectedRoute := uuid.New().String()
		expectedType := uuid.New().String()
		var expectedErr error
		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, originalTs)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, expectedRoute)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.MetricTagsKey, map[string]string{
			"key": "value",
		})

		expectedTags := map[string]string{
			"route":  expectedRoute,
			"status": "ok",
			"type":   expectedType,
			"key":    "value",
			"code":   strconv.Itoa(http.StatusOK),
		}

		mockMetricsReporter.EXPECT().ReportSummary(ResponseTime, expectedTags, gomock.Any())

		ReportTimingFromCtx(ctx, []Reporter{mockMetricsReporter}, expectedType, expectedErr)
	})

	t.Run("test-tags-not-correct-type", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockMetricsReporter := mocks.NewMockReporter(ctrl)

		originalTs := time.Now().UnixNano()
		expectedRoute := uuid.New().String()
		expectedType := uuid.New().String()
		var expectedErr error
		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, originalTs)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, expectedRoute)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.MetricTagsKey, "not-map")

		expectedTags := map[string]string{
			"route":  expectedRoute,
			"status": "ok",
			"type":   expectedType,
			"code":   strconv.Itoa(http.StatusOK),
		}

		mockMetricsReporter.EXPECT().ReportSummary(ResponseTime, expectedTags, gomock.Any())

		ReportTimingFromCtx(ctx, []Reporter{mockMetricsReporter}, expectedType, expectedErr)
	})

	t.Run("test-failed-route-with-pitaya-error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockMetricsReporter := mocks.NewMockReporter(ctrl)

		originalTs := time.Now().UnixNano()
		expectedRoute := uuid.New().String()
		expectedType := uuid.New().String()
		expectedErr := apierrors.NotFound("GAME-404", "error", "")
		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, originalTs)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, expectedRoute)

		mockMetricsReporter.EXPECT().ReportSummary(ResponseTime, map[string]string{
			"route":  expectedRoute,
			"status": "failed",
			"type":   expectedType,
			"code":   strconv.Itoa(http.StatusNotFound),
		}, gomock.Any())

		ReportTimingFromCtx(ctx, []Reporter{mockMetricsReporter}, expectedType, expectedErr)
	})

	t.Run("test-failed-route", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockMetricsReporter := mocks.NewMockReporter(ctrl)

		originalTs := time.Now().UnixNano()
		expectedRoute := uuid.New().String()
		expectedType := uuid.New().String()
		expectedErr := errors.New("error")
		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, originalTs)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, expectedRoute)

		mockMetricsReporter.EXPECT().ReportSummary(ResponseTime, map[string]string{
			"route":  expectedRoute,
			"status": "failed",
			"type":   expectedType,
			"code":   strconv.Itoa(http.StatusInternalServerError),
		}, gomock.Any())

		ReportTimingFromCtx(ctx, []Reporter{mockMetricsReporter}, expectedType, expectedErr)
	})
}

func TestReportMessageProcessDelayFromCtx(t *testing.T) {
	t.Run("test-tags", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockMetricsReporter := mocks.NewMockReporter(ctrl)

		originalTs := time.Now().UnixNano()
		expectedRoute := uuid.New().String()
		expectedType := uuid.New().String()
		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, originalTs)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, expectedRoute)
		ctx = pcontext.AddToPropagateCtx(ctx, constants.MetricTagsKey, map[string]string{
			"key": "value",
		})

		expectedTags := map[string]string{
			"route": expectedRoute,
			"type":  expectedType,
			"key":   "value",
		}

		mockMetricsReporter.EXPECT().ReportSummary(ProcessDelay, expectedTags, gomock.Any())

		ReportMessageProcessDelayFromCtx(ctx, []Reporter{mockMetricsReporter}, expectedType)
	})
}

func TestContextMetricHelpersIgnoreMalformedContext(t *testing.T) {
	ctrl := gomock.NewController(t)
	reporter := mocks.NewMockReporter(ctrl)

	assert.NotPanics(t, func() {
		ReportTimingFromCtx(nil, []Reporter{reporter}, "request", nil)
		ReportTimingFromCtx(context.Background(), []Reporter{reporter}, "request", nil)
		ReportMessageProcessDelayFromCtx(nil, []Reporter{reporter}, "request")
		ReportMessageProcessDelayFromCtx(context.Background(), []Reporter{reporter}, "request")

		ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, "not-an-int64")
		ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, 42)
		ReportTimingFromCtx(ctx, []Reporter{reporter}, "request", nil)
		ReportMessageProcessDelayFromCtx(ctx, []Reporter{reporter}, "request")
	})
}

func TestReportPoolGoDeadlinesUsesFallbackForMissingRoute(t *testing.T) {
	ctrl := gomock.NewController(t)
	reporter := mocks.NewMockReporter(ctrl)
	reporter.EXPECT().ReportCount(PoolGoDeadlines, map[string]string{
		"route": "nil", "second": "3", "pool": "workers",
	}, float64(1))

	ReportPoolGoDeadlines(nil, "workers", 3, []Reporter{reporter})
	ReportPoolGoDeadlines(context.Background(), "workers", 3, []Reporter{reporter})
}

func TestTagsFromContextConvertsJSONCompatibleMaps(t *testing.T) {
	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.MetricTagsKey, map[string]any{
		"region": "eu", "tenant": "blue",
	})
	assert.Equal(t, map[string]string{"region": "eu", "tenant": "blue"}, tagsFromContext(ctx))

	ctx = pcontext.AddToPropagateCtx(context.Background(), constants.MetricTagsKey, map[string]any{
		"region": "eu", "attempt": 2,
	})
	assert.Empty(t, tagsFromContext(ctx))
}

func TestGetTagsKeepsFrameworkDimensionsAuthoritative(t *testing.T) {
	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.MetricTagsKey, map[string]string{
		"route":  "forged.route",
		"status": "forged",
		"region": "eu",
	})

	tags := getTags(ctx, map[string]string{"route": "match.join", "status": "ok"})

	assert.Equal(t, map[string]string{
		"route":  "match.join",
		"status": "ok",
		"region": "eu",
	}, tags)
}

func TestMetricHelpersLogReporterErrors(t *testing.T) {
	core, observed := observer.New(zapcore.ErrorLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	reporter := errorReporter{err: errors.New("backend unavailable")}
	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, time.Now().UnixNano())
	ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, "match.join")

	ReportTimingFromCtx(ctx, []Reporter{reporter}, "request", nil)
	ReportPoolGoDeadlines(ctx, "default", 1, []Reporter{reporter})
	ReportMessageProcessDelayFromCtx(ctx, []Reporter{reporter}, "request")
	ReportNumberOfConnectedClients([]Reporter{reporter}, 1)
	reportSysMetrics([]Reporter{reporter})
	ReportExceededRateLimiting([]Reporter{reporter})

	entries := observed.FilterMessage("metrics reporter failed").All()
	assert.Len(t, entries, 8)
	for _, entry := range entries {
		assert.Equal(t, "backend unavailable", entry.ContextMap()["error"])
	}
}

func TestMetricHelpersHandleNilReporter(t *testing.T) {
	core, observed := observer.New(zapcore.ErrorLevel)
	previousLogger := logger.Zap
	logger.Zap = zap.New(core)
	t.Cleanup(func() { logger.Zap = previousLogger })

	ctx := pcontext.AddToPropagateCtx(context.Background(), constants.StartTimeKey, time.Now().UnixNano())
	ctx = pcontext.AddToPropagateCtx(ctx, constants.RouteKey, "match.join")
	reporters := []Reporter{nil}

	assert.NotPanics(t, func() {
		ReportTimingFromCtx(ctx, reporters, "request", nil)
		ReportPoolGoDeadlines(ctx, "default", 1, reporters)
		ReportMessageProcessDelayFromCtx(ctx, reporters, "request")
		ReportNumberOfConnectedClients(reporters, 1)
		reportSysMetrics(reporters)
		ReportExceededRateLimiting(reporters)
	})

	entries := observed.FilterMessage("metrics reporter failed").All()
	assert.Len(t, entries, 8)
	for _, entry := range entries {
		assert.Equal(t, errNilMetricsReporter.Error(), entry.ContextMap()["error"])
	}
}
