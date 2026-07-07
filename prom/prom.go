// Package prom is a Prometheus adapter for the go-net-dhcp/dhcp
// server. It implements dhcp.Metrics with two collectors:
//
//   - dhcpv4_packets_total{outcome}    — every inbound packet, labelled
//                                        by what the server did with it
//   - dhcpv4_handle_duration_seconds   — parse + decide + send latency
//
// Keeping the Prometheus dependency in this separate package means the
// core dhcp library stays free of third-party imports; consumers that
// want metrics opt in by importing prom and passing the returned
// *Metrics as Options.Metrics.
package prom

import (
	"github.com/go-net-dhcp/dhcp"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is a Prometheus-backed dhcp.Metrics.
type Metrics struct {
	packets *prometheus.CounterVec
	dur     prometheus.Histogram
}

// Compile-time assertion that *Metrics satisfies dhcp.Metrics.
var _ dhcp.Metrics = (*Metrics)(nil)

// New builds the collectors and registers them with reg. When reg is
// nil, prometheus.DefaultRegisterer is used. It returns an error when
// registration fails (e.g. a collector with the same name is already
// registered).
func New(reg prometheus.Registerer) (*Metrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	m := &Metrics{
		packets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dhcpv4_packets_total",
			Help: "Total DHCPv4 packets processed, labelled by outcome (offer|ack|nak|drop_parse_err|drop_unknown_mac|drop_decide_err|drop_unsupported|send_err).",
		}, []string{"outcome"}),
		dur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "dhcpv4_handle_duration_seconds",
			Help:    "Parse + Decide + send latency per inbound packet.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	if err := reg.Register(m.packets); err != nil {
		return nil, err
	}
	if err := reg.Register(m.dur); err != nil {
		return nil, err
	}
	return m, nil
}

// RecordPacket bumps the packets-total counter for outcome.
func (m *Metrics) RecordPacket(outcome string) {
	m.packets.WithLabelValues(outcome).Inc()
}

// RecordHandleDuration observes the per-packet latency in seconds.
func (m *Metrics) RecordHandleDuration(seconds float64) {
	m.dur.Observe(seconds)
}
