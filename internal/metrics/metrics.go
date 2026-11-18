// Package metrics defines the gateway's Prometheus metrics.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	Registry     *prometheus.Registry
	Activities   *prometheus.CounterVec
	AuthFailures *prometheus.CounterVec
	Turns        *prometheus.CounterVec
	TurnDuration *prometheus.HistogramVec
	FirstOutput  *prometheus.HistogramVec
	Connector    *prometheus.HistogramVec
	InFlight     prometheus.Gauge
	// HTTPRequests and HTTPDuration cover every /api/messages response by status code, so the
	// success rate can drive alerts and canary analysis.
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	BuildInfo    *prometheus.GaugeVec
}

func New() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		Registry: r,
		Activities: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teams_a2a_activities_total", Help: "Inbound Bot Service activities by route, type and result.",
		}, []string{"route", "type", "result"}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teams_a2a_auth_failures_total", Help: "Rejected inbound requests by HTTP status.",
		}, []string{"status"}),
		Turns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teams_a2a_turns_total", Help: "Completed agent turns by route and outcome.",
		}, []string{"route", "outcome"}),
		TurnDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "teams_a2a_turn_duration_seconds", Help: "End-to-end agent turn duration.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
		}, []string{"route"}),
		FirstOutput: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "teams_a2a_time_to_first_output_seconds", Help: "Time until the agent produced its first answer text.",
			Buckets: []float64{0.25, 0.5, 1, 2, 3, 5, 10, 20, 60},
		}, []string{"route"}),
		Connector: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "teams_a2a_connector_request_seconds", Help: "Bot Connector API calls by operation and status code.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op", "code"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "teams_a2a_turns_in_flight", Help: "Agent turns currently running.",
		}),
	}
	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "teams_a2a_http_requests_total", Help: "Bot Service requests to /api/messages by HTTP status.",
	}, []string{"code"})
	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "teams_a2a_http_request_duration_seconds", Help: "Time to answer Bot Service requests to /api/messages.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"code"})
	m.BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "teams_a2a_build_info", Help: "The running gateway version (always 1).",
	}, []string{"version"})
	r.MustRegister(m.HTTPRequests, m.HTTPDuration, m.BuildInfo, m.Activities, m.AuthFailures, m.Turns, m.TurnDuration, m.FirstOutput, m.Connector, m.InFlight,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) ObserveConnector(op string, status int, d time.Duration) {
	m.Connector.WithLabelValues(op, strconv.Itoa(status)).Observe(d.Seconds())
}
