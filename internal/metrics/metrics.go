// Package metrics mendefinisikan collector Prometheus (arsitektur §9). Satu
// Registry dipakai /metrics dan karhutla_metrics().
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics collector server. Method aman dipanggil pada nil (tanpa metrics).
type Metrics struct {
	Registry *prometheus.Registry

	queries   *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	rows      *prometheus.CounterVec
	bytes     *prometheus.CounterVec
	authFails *prometheus.CounterVec
}

// Counts sumber gauge sesi: jumlah sesi per source dan statement aktif.
type Counts func() (bySource map[string]int, statements int)

// New membuat Metrics beserta collector Go/process.
// ponytail: label user tanpa batas kardinalitas (sesuai spec §9); agregasi
// atau allowlist bila jumlah user besar.
func New(counts Counts) *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		queries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "karhutla_queries_total", Help: "Statement selesai per user dan status gRPC.",
		}, []string{"user", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "karhutla_query_duration_seconds", Help: "Durasi statement.",
			Buckets: []float64{.005, .025, .1, .5, 1, 2.5, 10, 30, 60, 300, 1800},
		}, []string{"status"}),
		rows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "karhutla_rows_sent_total", Help: "Baris dikirim ke klien.",
		}, []string{"user"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "karhutla_bytes_sent_total", Help: "Byte Arrow (buffer) dikirim ke klien.",
		}, []string{"user"}),
		authFails: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "karhutla_auth_failures_total", Help: "Autentikasi gagal per alasan.",
		}, []string{"reason"}),
	}
	m.Registry.MustRegister(m.queries, m.duration, m.rows, m.bytes, m.authFails,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		sessionCollector(counts))
	return m
}

// Statement mencatat statement selesai.
func (m *Metrics) Statement(user, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.queries.WithLabelValues(user, status).Inc()
	m.duration.WithLabelValues(status).Observe(d.Seconds())
}

// Sent mencatat baris & byte yang dikirim.
func (m *Metrics) Sent(user string, rows, bytes int64) {
	if m == nil {
		return
	}
	m.rows.WithLabelValues(user).Add(float64(rows))
	m.bytes.WithLabelValues(user).Add(float64(bytes))
}

// AuthFailure mencatat autentikasi gagal; reason harus dari himpunan tetap.
func (m *Metrics) AuthFailure(reason string) {
	if m == nil {
		return
	}
	m.authFails.WithLabelValues(reason).Inc()
}

var (
	activeSessions   = prometheus.NewDesc("karhutla_active_sessions", "Sesi di registry per source.", []string{"source"}, nil)
	activeStatements = prometheus.NewDesc("karhutla_active_statements", "Statement yang sedang berjalan.", nil, nil)
)

type countsCollector Counts

func sessionCollector(c Counts) prometheus.Collector { return countsCollector(c) }

func (c countsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- activeSessions
	ch <- activeStatements
}

func (c countsCollector) Collect(ch chan<- prometheus.Metric) {
	bySource, stmts := c()
	for src, n := range bySource {
		ch <- gauge(activeSessions, float64(n), src)
	}
	ch <- gauge(activeStatements, float64(stmts))
}

func gauge(d *prometheus.Desc, v float64, labels ...string) prometheus.Metric {
	m, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, v, labels...)
	if err != nil {
		return prometheus.NewInvalidMetric(d, err)
	}
	return m
}
