package dhcp

import (
	"net/netip"
	"testing"
)

// TestNopMetrics exercises the default no-op metrics sink and the
// metricsOrNop selector on both branches.
func TestNopMetrics(t *testing.T) {
	var n nopMetrics
	n.RecordPacket(OutcomeOffer)
	n.RecordHandleDuration(0.5)

	if _, ok := metricsOrNop(nil).(nopMetrics); !ok {
		t.Error("metricsOrNop(nil) should return nopMetrics")
	}
	custom := &countingMetrics{}
	if got := metricsOrNop(custom); got != custom {
		t.Error("metricsOrNop should pass a non-nil Metrics through unchanged")
	}
}

// countingMetrics is a test Metrics that tallies outcomes.
type countingMetrics struct {
	packets   map[string]int
	durations int
}

func (m *countingMetrics) RecordPacket(outcome string) {
	if m.packets == nil {
		m.packets = map[string]int{}
	}
	m.packets[outcome]++
}
func (m *countingMetrics) RecordHandleDuration(float64) { m.durations++ }

func TestMessageType_And_RequestedIP_Absent(t *testing.T) {
	// Empty options map: both accessors return their zero values.
	p := &Packet{Options: map[byte][]byte{}}
	if p.MessageType() != 0 {
		t.Errorf("MessageType() = %d, want 0 when absent", p.MessageType())
	}
	if p.RequestedIP().IsValid() {
		t.Error("RequestedIP() should be invalid when absent")
	}
	// Present but zero-length message-type value → still 0.
	p.Options[optMessageType] = []byte{}
	if p.MessageType() != 0 {
		t.Errorf("MessageType() = %d, want 0 for empty value", p.MessageType())
	}
	// Requested-IP present but wrong length → invalid.
	p.Options[optRequestedIP] = []byte{1, 2, 3}
	if p.RequestedIP().IsValid() {
		t.Error("RequestedIP() should be invalid for a non-4-byte value")
	}
}

func TestMACString_Fallbacks(t *testing.T) {
	// Hlen == 0 falls back to 6 bytes.
	p := &Packet{Hlen: 0}
	p.Chaddr = [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	if got := p.MACString(); got != "01:02:03:04:05:06" {
		t.Errorf("Hlen=0 MACString = %q, want 01:02:03:04:05:06", got)
	}
	// Hlen > 16 also falls back to 6 bytes.
	p.Hlen = 17
	if got := p.MACString(); got != "01:02:03:04:05:06" {
		t.Errorf("Hlen=17 MACString = %q, want 01:02:03:04:05:06", got)
	}
}

func TestAppendOption_Truncates(t *testing.T) {
	big := make([]byte, 300)
	out := appendOption(nil, optDomainName, big)
	// code + length byte + 255 value bytes
	if len(out) != 2+255 {
		t.Errorf("appendOption emitted %d bytes, want %d", len(out), 2+255)
	}
	if out[1] != 255 {
		t.Errorf("length byte = %d, want 255", out[1])
	}
}

func TestParse_PadOption(t *testing.T) {
	b := make([]byte, bootpHeaderSize+4)
	b[0] = opBootRequest
	b[2] = 6
	copy(b[bootpHeaderSize:bootpHeaderSize+4], magicCookie[:])
	// options: pad, pad, message-type, end
	b = append(b, optPad, optPad, optMessageType, 1, MsgDiscover, optEnd)
	p, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse with pad bytes: %v", err)
	}
	if p.MessageType() != MsgDiscover {
		t.Errorf("MessageType = %d, want DISCOVER after pad bytes", p.MessageType())
	}
}

func TestBuildReply_Errors(t *testing.T) {
	if _, err := BuildReply(nil, MsgOffer, netip.MustParseAddr("10.0.0.1"), Lease{}); err == nil {
		t.Error("expected error for nil request")
	}
	req := &Packet{}
	if _, err := BuildReply(req, MsgOffer, netip.Addr{}, Lease{}); err == nil {
		t.Error("expected error for invalid server identifier")
	}
	if _, err := BuildReply(req, MsgOffer, netip.MustParseAddr("2001:db8::1"), Lease{}); err == nil {
		t.Error("expected error for IPv6 server identifier")
	}
}

func TestDecide_NilPacket(t *testing.T) {
	if _, err := Decide(nil, Options{}); err == nil {
		t.Error("expected error for nil packet")
	}
}

// TestDecide_BuildReplyErrors drives the three BuildReply error
// returns inside Decide by handing it an invalid ServerIP (Decide
// does not pre-validate Options, so BuildReply fails downstream).
func TestDecide_BuildReplyErrors(t *testing.T) {
	valid := SourceFn(func(string) (Lease, bool) {
		return Lease{Yiaddr: netip.MustParseAddr("10.0.0.42"), SubnetMaskBits: 24}, true
	})
	opts := Options{Interface: "br0", ServerIP: netip.Addr{}, Source: valid}

	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}

	// DISCOVER → OFFER build fails.
	pkt, _ := Parse(buildDiscover(t, 1, mac, false, netip.Addr{}))
	if _, err := Decide(pkt, opts); err == nil {
		t.Error("expected OFFER BuildReply error")
	}

	// REQUEST matching → ACK build fails.
	pkt, _ = Parse(buildDiscover(t, 1, mac, false, netip.MustParseAddr("10.0.0.42")))
	pkt.Options[optMessageType] = []byte{MsgRequest}
	if _, err := Decide(pkt, opts); err == nil {
		t.Error("expected ACK BuildReply error")
	}

	// REQUEST mismatching → NAK build fails.
	pkt, _ = Parse(buildDiscover(t, 1, mac, false, netip.MustParseAddr("10.0.0.99")))
	pkt.Options[optMessageType] = []byte{MsgRequest}
	if _, err := Decide(pkt, opts); err == nil {
		t.Error("expected NAK BuildReply error")
	}
}
