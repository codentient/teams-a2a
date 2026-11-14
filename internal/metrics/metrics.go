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
	r.MustRegister(m.Activities, m.AuthFailures, m.Turns, m.TurnDuration, m.FirstOutput, m.Connector, m.InFlight,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) ObserveConnector(op string, status int, d time.Duration) {
	m.Connector.WithLabelValues(op, strconv.Itoa(status)).Observe(d.Seconds())
}
