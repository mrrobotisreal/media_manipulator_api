package retention

// MetricsSink is the slice of *metrics.Registry the sweeper reports to. It is
// declared here (not imported) so internal/retention stays free of the metrics
// package; *metrics.Registry satisfies it.
type MetricsSink interface {
	RetentionSwept(kind string, n int)
	RetentionWouldDelete(kind string, n int)
	RetentionSweepError(kind string)
	RetentionGaveUp(kind string)
	SetRetentionBacklog(due, gaveUp int64)
}

type metricsMeter struct{ m MetricsSink }

// NewMeter adapts a metrics sink to Meter (nil sink = NopMeter).
func NewMeter(m MetricsSink) Meter {
	if m == nil {
		return NopMeter{}
	}
	return metricsMeter{m: m}
}

func (a metricsMeter) Swept(k string, n int)       { a.m.RetentionSwept(k, n) }
func (a metricsMeter) WouldDelete(k string, n int) { a.m.RetentionWouldDelete(k, n) }
func (a metricsMeter) Errored(k string)            { a.m.RetentionSweepError(k) }
func (a metricsMeter) GaveUp(k string)             { a.m.RetentionGaveUp(k) }
func (a metricsMeter) Backlog(d, g int64)          { a.m.SetRetentionBacklog(d, g) }
