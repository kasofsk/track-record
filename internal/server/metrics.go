package server

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics are the server's Prometheus series. Every label has few values,
// none of them the client's to choose freely: a record_namespace is the one
// its token was minted for, never the one in the path; a name is one a writer
// gave a pass; a route is one of the server's patterns, which name the
// method, or empty for a request none matched; and a code is one the server
// answered with. The namespace is record_namespace because a scrape in
// Kubernetes labels every series with the pod's namespace, and Prometheus
// would rename a label of ours that collided with it.
type metrics struct {
	requests     *prometheus.HistogramVec
	hits         *prometheus.CounterVec
	misses       *prometheus.CounterVec
	savedSeconds *prometheus.CounterVec
	passes       *prometheus.CounterVec
	passSeconds  *prometheus.CounterVec
	sweptRecords prometheus.Counter
	sweptLogs    prometheus.Counter
}

// newMetrics returns the server's series, registered with reg unless it is
// nil.
func newMetrics(reg prometheus.Registerer) *metrics {
	f := promauto.With(reg)
	return &metrics{
		requests: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "track_record_http_request_duration_seconds",
			Help: "How long the server took to answer a request, by route and status code.",
		}, []string{"route", "code"}),
		hits: f.NewCounterVec(prometheus.CounterOpts{
			Name: "track_record_hits_total",
			Help: "Lookups that found a pass on record.",
		}, []string{"record_namespace", "name"}),
		misses: f.NewCounterVec(prometheus.CounterOpts{
			Name: "track_record_misses_total",
			Help: "Lookups that found no pass on record. The server does not know a missed command's name.",
		}, []string{"record_namespace"}),
		savedSeconds: f.NewCounterVec(prometheus.CounterOpts{
			Name: "track_record_saved_seconds_total",
			Help: "For each hit, how long the pass found took to run: the time the hit saved. A record whose writer did not say adds nothing.",
		}, []string{"record_namespace", "name"}),
		passes: f.NewCounterVec(prometheus.CounterOpts{
			Name: "track_record_passes_total",
			Help: "Passes put on record, including those that found one already there.",
		}, []string{"record_namespace", "name"}),
		passSeconds: f.NewCounterVec(prometheus.CounterOpts{
			Name: "track_record_pass_seconds_total",
			Help: "How long each pass put on record took to run. A put that does not say adds nothing.",
		}, []string{"record_namespace", "name"}),
		sweptRecords: f.NewCounter(prometheus.CounterOpts{
			Name: "track_record_swept_records_total",
			Help: "Records the sweeper deleted, having expired or been written by a revoked token.",
		}),
		sweptLogs: f.NewCounter(prometheus.CounterOpts{
			Name: "track_record_swept_logs_total",
			Help: "Logs the sweeper deleted, no live record referring to them.",
		}),
	}
}

func (m *metrics) request(route string, status int, took time.Duration) {
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Observe(took.Seconds())
}

func (m *metrics) hit(namespace, name string, durationMs *int64) {
	m.hits.WithLabelValues(namespace, name).Inc()
	if durationMs != nil {
		m.savedSeconds.WithLabelValues(namespace, name).Add(seconds(*durationMs))
	}
}

func (m *metrics) miss(namespace string) {
	m.misses.WithLabelValues(namespace).Inc()
}

func (m *metrics) pass(namespace, name string, durationMs *int64) {
	m.passes.WithLabelValues(namespace, name).Inc()
	if durationMs != nil {
		m.passSeconds.WithLabelValues(namespace, name).Add(seconds(*durationMs))
	}
}

func seconds(ms int64) float64 { return float64(ms) / 1000 }
