package dhcp

// Metrics receives per-packet telemetry from a running server. It is
// deliberately tiny and dependency-free so the core library imports
// nothing third-party. Pass an implementation via Options.Metrics ;
// a nil Metrics is replaced by a no-op.
//
// A ready-made Prometheus adapter lives in the sibling package
// github.com/go-net-dhcp/dhcp/prom.
//
// Implementations must be safe for concurrent use — the server calls
// them from its single receive goroutine today, but that is not part
// of the contract.
type Metrics interface {
	// RecordPacket is called once per inbound packet with the
	// outcome (one of the Outcome* constants).
	RecordPacket(outcome string)
	// RecordHandleDuration observes the parse+decide+send latency,
	// in seconds, for one inbound packet.
	RecordHandleDuration(seconds float64)
}

// Outcome labels passed to Metrics.RecordPacket. Bounded cardinality
// (no per-MAC labels) so a Prometheus counter stays cheap.
const (
	OutcomeOffer           = "offer"            // sent an OFFER
	OutcomeAck             = "ack"              // sent an ACK
	OutcomeNak             = "nak"              // sent a NAK
	OutcomeDropParseErr    = "drop_parse_err"   // malformed wire packet
	OutcomeDropUnknownMAC  = "drop_unknown_mac" // Source.Resolve returned false
	OutcomeDropDecideErr   = "drop_decide_err"  // lease validation / BuildReply failed
	OutcomeDropUnsupported = "drop_unsupported" // message type we don't answer
	OutcomeSendErr         = "send_err"         // wire-side write failed
)

// nopMetrics is the default Metrics used when Options.Metrics is nil.
type nopMetrics struct{}

func (nopMetrics) RecordPacket(string)          {}
func (nopMetrics) RecordHandleDuration(float64) {}

// metricsOrNop returns m, or a no-op sink when m is nil.
func metricsOrNop(m Metrics) Metrics {
	if m == nil {
		return nopMetrics{}
	}
	return m
}
