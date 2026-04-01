package checks

import "github.com/prometheus/client_golang/prometheus"

var (
	probeRuns = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_runs_total",
			Help: "Total probe runs by check type.",
		},
		[]string{"check_type"},
	)
	probeWriteErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_write_errors_total",
			Help: "Total probe write errors by check type.",
		},
		[]string{"check_type"},
	)
)

func init() {
	prometheus.MustRegister(probeRuns, probeWriteErrors)
}

func recordProbe(checkType string) {
	probeRuns.WithLabelValues(checkType).Inc()
}

func recordProbeWriteError(checkType string) {
	probeWriteErrors.WithLabelValues(checkType).Inc()
}
