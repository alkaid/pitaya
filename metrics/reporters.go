package metrics

type reporterMgr struct {
	reporters []Reporter
}

func NewReporterMgr(reporters []Reporter) Reporter {
	return &reporterMgr{reporters: reporters}
}

func (r *reporterMgr) ReportCount(metric string, tags map[string]string, count float64) error {
	for _, reporter := range r.reporters {
		reporter.ReportCount(metric, tags, count)
	}
	return nil
}

func (r *reporterMgr) ReportSummary(metric string, tags map[string]string, value float64) error {
	for _, reporter := range r.reporters {
		reporter.ReportSummary(metric, tags, value)
	}
	return nil
}

func (r *reporterMgr) ReportHistogram(metric string, tags map[string]string, value float64) error {
	for _, reporter := range r.reporters {
		reporter.ReportHistogram(metric, tags, value)
	}
	return nil
}

func (r *reporterMgr) ReportGauge(metric string, tags map[string]string, value float64) error {
	for _, reporter := range r.reporters {
		reporter.ReportGauge(metric, tags, value)
	}
	return nil
}
