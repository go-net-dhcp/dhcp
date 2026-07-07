package prom

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestNew_RegistersAndRecords(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	m.RecordPacket("offer")
	m.RecordPacket("offer")
	m.RecordPacket("nak")
	m.RecordHandleDuration(0.01)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var offer, nak float64
	var sawHist bool
	for _, mf := range mfs {
		switch mf.GetName() {
		case "dhcpv4_packets_total":
			for _, met := range mf.GetMetric() {
				for _, lp := range met.GetLabel() {
					if lp.GetName() == "outcome" {
						switch lp.GetValue() {
						case "offer":
							offer = met.GetCounter().GetValue()
						case "nak":
							nak = met.GetCounter().GetValue()
						}
					}
				}
			}
		case "dhcpv4_handle_duration_seconds":
			sawHist = true
			if got := mf.GetMetric()[0].GetHistogram().GetSampleCount(); got != 1 {
				t.Errorf("histogram sample count = %d, want 1", got)
			}
		}
	}
	if offer != 2 {
		t.Errorf("offer counter = %v, want 2", offer)
	}
	if nak != 1 {
		t.Errorf("nak counter = %v, want 1", nak)
	}
	if !sawHist {
		t.Error("histogram not registered")
	}
}

func TestNew_NilRegistererUsesDefault(t *testing.T) {
	// Register against the default registerer, then unregister so the
	// test is repeatable and doesn't leak collectors.
	m, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if !prometheus.Unregister(m.packets) {
		t.Error("packets collector was not registered on the default registerer")
	}
	if !prometheus.Unregister(m.dur) {
		t.Error("dur collector was not registered on the default registerer")
	}
}

func TestNew_DuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(reg); err != nil {
		t.Fatalf("first New: %v", err)
	}
	// Second New on the same registry collides on the counter name.
	_, err := New(reg)
	if err == nil {
		t.Fatal("expected duplicate-registration error")
	}
	if !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "already") {
		t.Logf("got registration error: %v", err)
	}
}

// TestNew_DurationCollisionOnly forces the second Register (histogram)
// to fail while the first (counter) succeeds, covering that error path.
func TestNew_DurationCollisionOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	// Pre-register a histogram with the same name so only the second
	// Register call inside New fails.
	clash := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "dhcpv4_handle_duration_seconds",
		Help: "clash",
	})
	if err := reg.Register(clash); err != nil {
		t.Fatalf("pre-register clash: %v", err)
	}
	if _, err := New(reg); err == nil {
		t.Fatal("expected histogram-registration error")
	}
}
