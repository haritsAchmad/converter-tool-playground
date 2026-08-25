package app

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type metrics struct {
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	jobsTotal    *prometheus.CounterVec
	jobDuration  prometheus.Histogram
	rateLimited  prometheus.Counter
}

func newMetrics(reg *prometheus.Registry, queueLength func() float64) *metrics {
	f := promauto.With(reg)
	m := &metrics{
		httpRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "convertbox_http_requests_total",
			Help: "Total HTTP requests by method, normalized path, and status code.",
		}, []string{"method", "path", "status"}),
		httpDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "convertbox_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path"}),
		jobsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "convertbox_jobs_total",
			Help: "Total conversion jobs by terminal status and format pair.",
		}, []string{"status", "input_format", "output_format"}),
		jobDuration: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "convertbox_job_duration_seconds",
			Help:    "Conversion job processing duration in seconds, from dequeue to finish.",
			Buckets: prometheus.DefBuckets,
		}),
		rateLimited: f.NewCounter(prometheus.CounterOpts{
			Name: "convertbox_rate_limited_total",
			Help: "Total job submissions rejected by per-IP rate limiting.",
		}),
	}
	f.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "convertbox_queue_length",
		Help: "Current number of jobs waiting in the conversion queue.",
	}, queueLength)
	return m
}
