package metrics

import (
	"errors"
	"testing"
)

type errorReporter struct {
	err error
}

func (r errorReporter) ReportCount(string, map[string]string, float64) error     { return r.err }
func (r errorReporter) ReportSummary(string, map[string]string, float64) error   { return r.err }
func (r errorReporter) ReportHistogram(string, map[string]string, float64) error { return r.err }
func (r errorReporter) ReportGauge(string, map[string]string, float64) error     { return r.err }

func TestReporterManagerAggregatesErrors(t *testing.T) {
	first := errors.New("first reporter failed")
	second := errors.New("second reporter failed")
	manager := NewReporterMgr([]Reporter{
		errorReporter{},
		errorReporter{err: first},
		errorReporter{err: second},
	})

	tests := []struct {
		name   string
		report func() error
	}{
		{name: "count", report: func() error { return manager.ReportCount("metric", nil, 1) }},
		{name: "summary", report: func() error { return manager.ReportSummary("metric", nil, 1) }},
		{name: "histogram", report: func() error { return manager.ReportHistogram("metric", nil, 1) }},
		{name: "gauge", report: func() error { return manager.ReportGauge("metric", nil, 1) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.report()
			if !errors.Is(err, first) || !errors.Is(err, second) {
				t.Fatalf("manager error = %v, want both reporter errors", err)
			}
		})
	}
}

func TestReporterManagerReturnsNilWhenAllReportersSucceed(t *testing.T) {
	manager := NewReporterMgr([]Reporter{errorReporter{}, errorReporter{}})
	if err := manager.ReportGauge("metric", nil, 1); err != nil {
		t.Fatalf("manager error = %v, want nil", err)
	}
}

func TestReporterManagerReturnsErrorForNilReporter(t *testing.T) {
	manager := NewReporterMgr([]Reporter{nil})
	tests := []struct {
		name   string
		report func() error
	}{
		{name: "count", report: func() error { return manager.ReportCount("metric", nil, 1) }},
		{name: "summary", report: func() error { return manager.ReportSummary("metric", nil, 1) }},
		{name: "histogram", report: func() error { return manager.ReportHistogram("metric", nil, 1) }},
		{name: "gauge", report: func() error { return manager.ReportGauge("metric", nil, 1) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.report(); !errors.Is(err, errNilMetricsReporter) {
				t.Fatalf("manager error = %v, want %v", err, errNilMetricsReporter)
			}
		})
	}
}
