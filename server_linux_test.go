//go:build linux

package dhcp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ---- test helpers -----------------------------------------------------------

// syncMetrics is a concurrency-safe dhcp.Metrics for the recv-loop tests.
type syncMetrics struct {
	mu        sync.Mutex
	packets   map[string]int
	durations int
}

func (m *syncMetrics) RecordPacket(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.packets == nil {
		m.packets = map[string]int{}
	}
	m.packets[outcome]++
}
func (m *syncMetrics) RecordHandleDuration(float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations++
}
func (m *syncMetrics) count(outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.packets[outcome]
}

var errInjected = errors.New("injected")

// okSyscalls returns a syscall seam where every call succeeds and the
// socket is adopted as conn.
func okSyscalls(conn net.PacketConn) linuxSyscalls {
	return linuxSyscalls{
		socket:           func(_, _, _ int) (int, error) { return 3, nil },
		setsockoptInt:    func(_, _, _, _ int) error { return nil },
		setsockoptString: func(_, _, _ int, _ string) error { return nil },
		bind:             func(_ int, _ unix.Sockaddr) error { return nil },
		closeFD:          func(_ int) error { return nil },
		newConn:          func(_ int, _ string) (net.PacketConn, error) { return conn, nil },
	}
}

func testOptions(src Source, m Metrics) Options {
	return Options{
		Interface: "lo",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source:    src,
		Metrics:   m,
	}
}

// notUDPConn is a net.PacketConn that is deliberately not a *net.UDPConn
// so listen()'s type assertion takes the failure branch.
type notUDPConn struct{ net.PacketConn }

func (notUDPConn) Close() error { return nil }

// ---- listen() branch coverage ----------------------------------------------

func TestListen_SocketError(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys.socket = func(_, _, _ int) (int, error) { return -1, errInjected }
	if _, err := s.listen(); err == nil {
		t.Fatal("expected socket error")
	}
}

func TestListen_SetsockoptErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*linuxSyscalls)
	}{
		{"SO_REUSEADDR", func(sc *linuxSyscalls) {
			sc.setsockoptInt = func(_, _, opt, _ int) error {
				if opt == unix.SO_REUSEADDR {
					return errInjected
				}
				return nil
			}
		}},
		{"SO_BROADCAST", func(sc *linuxSyscalls) {
			sc.setsockoptInt = func(_, _, opt, _ int) error {
				if opt == unix.SO_BROADCAST {
					return errInjected
				}
				return nil
			}
		}},
		{"SO_BINDTODEVICE", func(sc *linuxSyscalls) {
			sc.setsockoptString = func(_, _, _ int, _ string) error { return errInjected }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := NewLinuxServer(testOptions(nopSource(), nil))
			var closed bool
			s.sys = okSyscalls(nil)
			s.sys.closeFD = func(_ int) error { closed = true; return nil }
			c.mut(&s.sys)
			if _, err := s.listen(); err == nil {
				t.Fatal("expected sockopt error")
			}
			if !closed {
				t.Error("fd should have been closed on the error path")
			}
		})
	}
}

func TestListen_BindError(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys = okSyscalls(nil)
	s.sys.bind = func(_ int, _ unix.Sockaddr) error { return errInjected }
	if _, err := s.listen(); err == nil {
		t.Fatal("expected bind error")
	}
}

func TestListen_NewConnError(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys = okSyscalls(nil)
	s.sys.newConn = func(_ int, _ string) (net.PacketConn, error) { return nil, errInjected }
	if _, err := s.listen(); err == nil {
		t.Fatal("expected adopt-fd error")
	}
}

func TestListen_NotUDPConn(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys = okSyscalls(notUDPConn{})
	if _, err := s.listen(); err == nil {
		t.Fatal("expected unexpected-conn-type error")
	}
}

func TestListen_Success(t *testing.T) {
	uc := mustUDPConn(t)
	defer uc.Close()
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys = okSyscalls(uc)
	got, err := s.listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if got != uc {
		t.Errorf("listen returned %p, want %p", got, uc)
	}
}

// ---- Run() branch coverage --------------------------------------------------

func TestRun_NilCtx(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	if err := s.Run(nil); err == nil {
		t.Error("expected nil-ctx error")
	}
}

func TestRun_ListenError(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys.socket = func(_, _, _ int) (int, error) { return -1, errInjected }
	if err := s.Run(context.Background()); err == nil {
		t.Error("expected bind error from Run")
	}
}

// TestRun_RecvLoopAndCancel drives a DISCOVER through the full recv
// loop (Read → handle → Decide → send) and then cancels ctx, taking
// the ctx.Err() exit branch.
func TestRun_RecvLoopAndCancel(t *testing.T) {
	lc := mustUDPConn(t) // the server's "socket"
	rcv := mustUDPConn(t)
	defer rcv.Close()

	var seen string
	var mu sync.Mutex
	src := SourceFn(func(mac string) (Lease, bool) {
		mu.Lock()
		seen = mac
		mu.Unlock()
		return Lease{
			Yiaddr:         netip.MustParseAddr("10.0.0.42"),
			SubnetMaskBits: 24,
			Router:         netip.MustParseAddr("10.0.0.1"),
			LeaseTime:      time.Hour,
		}, true
	})
	m := &syncMetrics{}
	s, _ := NewLinuxServer(testOptions(src, m))
	s.sys = okSyscalls(lc)
	s.dst = rcv.LocalAddr().(*net.UDPAddr) // retarget the broadcast at rcv

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// Send a DISCOVER to the server's socket.
	cli := mustUDPConn(t)
	defer cli.Close()
	mac := [6]byte{0x52, 0x54, 0xde, 0xad, 0xbe, 0xef}
	raw := buildDiscover(t, 0x12345678, mac, true, netip.Addr{})
	if _, err := cli.WriteToUDP(raw, lc.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("send DISCOVER: %v", err)
	}

	// Read the OFFER on the retargeted destination.
	_ = rcv.SetReadDeadline(time.Now().Add(2 * time.Second))
	rbuf := make([]byte, 1500)
	if _, _, err := rcv.ReadFromUDP(rbuf); err != nil {
		t.Fatalf("expected OFFER on rcv: %v", err)
	}
	if m.count(OutcomeOffer) != 1 {
		t.Errorf("offer metric = %d, want 1", m.count(OutcomeOffer))
	}
	mu.Lock()
	gotMAC := seen
	mu.Unlock()
	if gotMAC != "52:54:de:ad:be:ef" {
		t.Errorf("Source saw mac=%q", gotMAC)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != context.Canceled {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't unblock on cancel")
	}
}

// TestRun_ReadError closes the socket underneath a live Run (ctx still
// active) so ReadFromUDP fails and Run returns the read error path.
func TestRun_ReadError(t *testing.T) {
	lc := mustUDPConn(t)
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.sys = okSyscalls(lc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// Wait until Run has adopted the conn, then close it out from under it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c != nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-runErr:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("Run err = %v, want a read error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return on socket close")
	}
}

// ---- handle() outcome coverage ---------------------------------------------

func TestHandle_Outcomes(t *testing.T) {
	mac := [6]byte{0x52, 0x54, 0, 0, 0, 1}

	newServer := func(src Source) (*LinuxServer, *syncMetrics) {
		m := &syncMetrics{}
		s, _ := NewLinuxServer(testOptions(src, m))
		return s, m
	}

	t.Run("parse_err", func(t *testing.T) {
		s, m := newServer(nopSource())
		s.handle(nil, []byte{1, 2, 3})
		if m.count(OutcomeDropParseErr) != 1 {
			t.Error("want drop_parse_err")
		}
	})

	t.Run("decide_err", func(t *testing.T) {
		bad := SourceFn(func(string) (Lease, bool) { return Lease{}, true }) // invalid lease
		s, m := newServer(bad)
		s.handle(nil, buildDiscover(t, 1, mac, false, netip.Addr{}))
		if m.count(OutcomeDropDecideErr) != 1 {
			t.Error("want drop_decide_err")
		}
	})

	t.Run("unknown_mac", func(t *testing.T) {
		s, m := newServer(nopSource())
		s.handle(nil, buildDiscover(t, 1, mac, false, netip.Addr{}))
		if m.count(OutcomeDropUnknownMAC) != 1 {
			t.Error("want drop_unknown_mac")
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		// A RELEASE from a KNOWN MAC still drops (unsupported message
		// type), distinct from the unknown-MAC drop above.
		s, m := newServer(goodSource())
		s.handle(nil, buildRelease(t, mac))
		if m.count(OutcomeDropUnsupported) != 1 {
			t.Errorf("want drop_unsupported, got %d", m.count(OutcomeDropUnsupported))
		}
	})

	t.Run("send_err", func(t *testing.T) {
		s, m := newServer(goodSource())
		closed := mustUDPConn(t)
		_ = closed.Close() // writing to a closed conn fails
		s.handle(closed, buildDiscover(t, 1, mac, false, netip.Addr{}))
		if m.count(OutcomeSendErr) != 1 {
			t.Errorf("want send_err, got %d", m.count(OutcomeSendErr))
		}
	})

	t.Run("offer_ack_nak", func(t *testing.T) {
		rcv := mustUDPConn(t)
		defer rcv.Close()
		snd := mustUDPConn(t)
		defer snd.Close()
		s, m := newServer(goodSource())
		s.dst = rcv.LocalAddr().(*net.UDPAddr)

		// OFFER (DISCOVER)
		s.handle(snd, buildDiscover(t, 1, mac, false, netip.Addr{}))
		// ACK (REQUEST, matching IP)
		s.handle(snd, buildRequest(t, mac, netip.MustParseAddr("10.0.0.42")))
		// NAK (REQUEST, mismatching IP)
		s.handle(snd, buildRequest(t, mac, netip.MustParseAddr("10.0.0.99")))

		if m.count(OutcomeOffer) != 1 || m.count(OutcomeAck) != 1 || m.count(OutcomeNak) != 1 {
			t.Errorf("offer/ack/nak = %d/%d/%d, want 1/1/1",
				m.count(OutcomeOffer), m.count(OutcomeAck), m.count(OutcomeNak))
		}
		if m.durations != 3 {
			t.Errorf("handle durations = %d, want 3", m.durations)
		}
	})
}

// ---- misc method coverage ---------------------------------------------------

func TestServer_SetLoggerAndServerIP(t *testing.T) {
	s, _ := NewLinuxServer(testOptions(nopSource(), nil))
	s.SetLogger(slog.New(slog.NewTextHandler(&nullWriter{}, nil)))
	s.SetLogger(nil) // restores default
	if s.ServerIP() != netip.MustParseAddr("10.0.0.1") {
		t.Errorf("ServerIP = %v", s.ServerIP())
	}
}

func TestNewLinuxServer_BadOptions(t *testing.T) {
	if _, err := NewLinuxServer(Options{}); err == nil {
		t.Error("expected validation error")
	}
}

// ---- small fixtures ---------------------------------------------------------

type nullWriter struct{}

func (*nullWriter) Write(p []byte) (int, error) { return len(p), nil }

func nopSource() Source { return SourceFn(func(string) (Lease, bool) { return Lease{}, false }) }

func goodSource() Source {
	return SourceFn(func(string) (Lease, bool) {
		return Lease{
			Yiaddr:         netip.MustParseAddr("10.0.0.42"),
			SubnetMaskBits: 24,
			Router:         netip.MustParseAddr("10.0.0.1"),
			LeaseTime:      time.Hour,
		}, true
	})
}

func mustUDPConn(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	return c
}

func buildRequest(t *testing.T, mac [6]byte, requested netip.Addr) []byte {
	t.Helper()
	raw := buildDiscover(t, 1, mac, false, requested)
	pkt, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pkt.Options[optMessageType] = []byte{MsgRequest}
	// Re-encode to a REQUEST on the wire.
	return encode(t, pkt)
}

func buildRelease(t *testing.T, mac [6]byte) []byte {
	t.Helper()
	raw := buildDiscover(t, 1, mac, false, netip.Addr{})
	pkt, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pkt.Options[optMessageType] = []byte{MsgRelease}
	return encode(t, pkt)
}

// encode serialises a Packet's header + message-type + requested-ip
// options back to the wire for driving handle().
func encode(t *testing.T, p *Packet) []byte {
	t.Helper()
	buf := make([]byte, bootpHeaderSize+4, 300)
	buf[0] = p.Op
	buf[1] = htypeEthernet
	buf[2] = 6
	copy(buf[28:44], p.Chaddr[:])
	copy(buf[bootpHeaderSize:bootpHeaderSize+4], magicCookie[:])
	buf = appendOption(buf, optMessageType, p.Options[optMessageType])
	if v, ok := p.Options[optRequestedIP]; ok {
		buf = appendOption(buf, optRequestedIP, v)
	}
	buf = append(buf, optEnd)
	return buf
}
