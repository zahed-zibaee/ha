package api

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	lbRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lb_requests_total",
			Help: "Total LB requests by path and cache hit.",
		},
		[]string{"path", "cache_hit"},
	)
	lbLatencyMs = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "lb_latency_ms",
			Help:    "LB request latency in milliseconds.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
		},
		[]string{"path"},
	)
	lbErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lb_errors_total",
			Help: "Total LB errors by type.",
		},
		[]string{"type"},
	)
	checkRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "check_requests_total",
			Help: "Total check requests by redis status.",
		},
		[]string{"redis_status"},
	)
	checkLatencyMs = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "check_latency_ms",
			Help:    "Check request latency in milliseconds.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
		},
		[]string{"redis_status"},
	)
	checkTargets = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "check_targets_total",
			Help: "Number of targets returned by check endpoint.",
		},
		[]string{"redis_status"},
	)
)

func init() {
	prometheus.MustRegister(lbRequests, lbLatencyMs, lbErrors, checkRequests, checkLatencyMs, checkTargets)
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}
