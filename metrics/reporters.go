package metrics

import (
	"errors"
	"fmt"
)

var errNilMetricsReporter = errors.New("nil metrics reporter")

type reporterMgr struct {
	reporters []Reporter
}

func NewReporterMgr(reporters []Reporter) Reporter {
	return &reporterMgr{reporters: reporters}
}

func (r *reporterMgr) ReportCount(metric string, tags map[string]string, count float64) error {
	var errs []error
	for _, reporter := range r.reporters {
		if reporter == nil {
			errs = append(errs, errNilMetricsReporter)
			continue
		}
		if err := reporter.ReportCount(metric, tags, count); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", reporter, err))
		}
	}
	return errors.Join(errs...)
}

func (r *reporterMgr) ReportSummary(metric string, tags map[string]string, value float64) error {
	var errs []error
	for _, reporter := range r.reporters {
		if reporter == nil {
			errs = append(errs, errNilMetricsReporter)
			continue
		}
		if err := reporter.ReportSummary(metric, tags, value); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", reporter, err))
		}
	}
	return errors.Join(errs...)
}

func (r *reporterMgr) ReportHistogram(metric string, tags map[string]string, value float64) error {
	var errs []error
	for _, reporter := range r.reporters {
		if reporter == nil {
			errs = append(errs, errNilMetricsReporter)
			continue
		}
		if err := reporter.ReportHistogram(metric, tags, value); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", reporter, err))
		}
	}
	return errors.Join(errs...)
}

func (r *reporterMgr) ReportGauge(metric string, tags map[string]string, value float64) error {
	var errs []error
	for _, reporter := range r.reporters {
		if reporter == nil {
			errs = append(errs, errNilMetricsReporter)
			continue
		}
		if err := reporter.ReportGauge(metric, tags, value); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", reporter, err))
		}
	}
	return errors.Join(errs...)
}
