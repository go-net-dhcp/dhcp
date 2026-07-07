//go:build linux

package dhcp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// LinuxServer is the real DHCPv4 server. It binds UDP/67 on the
// configured Interface (via SO_BINDTODEVICE so only broadcast
// traffic from that bridge / VLAN is delivered to the socket) and
// replies via UDP/68 — broadcast to 255.255.255.255.
//
// Lifecycle : NewLinuxServer prepares the socket lazily ; Run does
// the actual bind + recv loop ; Run unblocks when ctx is cancelled
// or the socket errors out.
type LinuxServer struct {
	opts    Options
	metrics Metrics

	logger *slog.Logger

	// dst is the reply destination (global broadcast, UDP/68). It's
	// a field rather than a literal so tests can retarget the write
	// at a local listener and exercise the send path without needing
	// broadcast permission.
	dst *net.UDPAddr

	// sys holds the syscall funcs used to open the socket. Defaults
	// wire straight to unix.* ; tests override them to fault-inject
	// every error branch without CAP_NET_RAW.
	sys linuxSyscalls

	mu   sync.Mutex
	conn *net.UDPConn // populated by Run, cleared on shutdown
}

// linuxSyscalls is the seam over the raw socket calls listen() makes.
type linuxSyscalls struct {
	socket           func(domain, typ, proto int) (int, error)
	setsockoptInt    func(fd, level, opt, value int) error
	setsockoptString func(fd, level, opt int, value string) error
	bind             func(fd int, sa unix.Sockaddr) error
	closeFD          func(fd int) error
	newConn          func(fd int, name string) (net.PacketConn, error)
}

// defaultSyscalls wires the seam to the real kernel calls.
func defaultSyscalls() linuxSyscalls {
	return linuxSyscalls{
		socket:           unix.Socket,
		setsockoptInt:    unix.SetsockoptInt,
		setsockoptString: unix.SetsockoptString,
		bind:             unix.Bind,
		closeFD:          unix.Close,
		newConn: func(fd int, name string) (net.PacketConn, error) {
			// Hand the fd to the os/net stack so we get ReadFromUDP /
			// WriteToUDP ergonomics + the runtime poller. FilePacketConn
			// dup's the fd, so the original os.File is no longer needed.
			f := os.NewFile(uintptr(fd), name)
			c, err := net.FilePacketConn(f)
			_ = f.Close()
			return c, err
		},
	}
}

// NewLinuxServer validates opts and returns a server ready for Run.
// Doesn't open any sockets yet — that happens in Run so a caller
// can construct the server early in start-up and surface socket
// errors via the same `Run` channel as everything else.
func NewLinuxServer(opts Options) (*LinuxServer, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &LinuxServer{
		opts:    opts,
		metrics: metricsOrNop(opts.Metrics),
		logger:  slog.Default().With("component", "dhcp", "iface", opts.Interface),
		dst:     &net.UDPAddr{IP: net.IPv4bcast, Port: 68},
		sys:     defaultSyscalls(),
	}, nil
}

// SetLogger swaps the slog.Logger used for handler-loop diagnostics.
// Nil restores the package default.
func (s *LinuxServer) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default().With("component", "dhcp", "iface", s.opts.Interface)
	}
	s.logger = l
}

// Run binds the socket and processes packets until ctx is done.
// All transient errors (parse failures, source misses) are logged
// at debug level and the loop continues — only socket-fatal errors
// break out.
func (s *LinuxServer) Run(ctx contextLike) error {
	if ctx == nil {
		return errors.New("dhcp.LinuxServer.Run: nil ctx")
	}
	conn, err := s.listen()
	if err != nil {
		return fmt.Errorf("dhcp: bind :67 on %s: %w", s.opts.Interface, err)
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	// One goroutine closes the conn when ctx is done so the
	// blocking ReadFromUDP returns with an error and the recv loop
	// exits cleanly.
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		c := s.conn
		s.conn = nil
		s.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	}()

	s.logger.Info("dhcp listening", "iface", s.opts.Interface, "server_ip", s.opts.ServerIP)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Closed socket on ctx-cancel is the expected exit path.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("dhcp: read: %w", err)
		}
		s.handle(conn, buf[:n])
	}
}

// listen opens UDP/67 bound to the configured interface. We
// construct the socket manually (vs `net.ListenUDP`) so we can
// call SO_BINDTODEVICE + SO_BROADCAST + SO_REUSEADDR before the
// kernel commits the binding.
func (s *LinuxServer) listen() (*net.UDPConn, error) {
	fd, err := s.sys.socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}

	cleanup := func() { _ = s.sys.closeFD(fd) }

	if err := s.sys.setsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		cleanup()
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	if err := s.sys.setsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
		cleanup()
		return nil, fmt.Errorf("SO_BROADCAST: %w", err)
	}
	// SO_BINDTODEVICE pins the socket to the named kernel interface
	// — only frames arriving on that NIC / bridge / VLAN reach us,
	// and replies go out the same path. Requires CAP_NET_RAW.
	if err := s.sys.setsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, s.opts.Interface); err != nil {
		cleanup()
		return nil, fmt.Errorf("SO_BINDTODEVICE %s: %w", s.opts.Interface, err)
	}

	sa := &unix.SockaddrInet4{Port: 67} // INADDR_ANY because the device binding already restricts the path
	if err := s.sys.bind(fd, sa); err != nil {
		cleanup()
		return nil, fmt.Errorf("bind :67: %w", err)
	}

	c, err := s.sys.newConn(fd, fmt.Sprintf("dhcp-%s", s.opts.Interface))
	if err != nil {
		// newConn is responsible for the fd on error.
		return nil, fmt.Errorf("adopt fd: %w", err)
	}
	uc, ok := c.(*net.UDPConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("dhcp: unexpected conn type %T", c)
	}
	return uc, nil
}

// handle parses one inbound packet and runs Decide. Errors don't
// kill the server ; they get logged. All policy lives in Decide
// (build-tag-free) so unit tests cover the same code path the linux
// loop runs.
func (s *LinuxServer) handle(conn *net.UDPConn, raw []byte) {
	start := time.Now()
	defer func() { s.metrics.RecordHandleDuration(time.Since(start).Seconds()) }()

	pkt, err := Parse(raw)
	if err != nil {
		s.logger.Debug("parse failed", "err", err, "len", len(raw))
		s.metrics.RecordPacket(OutcomeDropParseErr)
		return
	}
	d, err := Decide(pkt, s.opts)
	if err != nil {
		s.logger.Warn("decide", "mac", d.MAC, "err", err)
		s.metrics.RecordPacket(OutcomeDropDecideErr)
		return
	}
	if d.Reply == nil {
		s.logger.Debug("dropped", "mac", d.MAC, "msg_type", pkt.MessageType())
		// Distinguish unknown-mac (Resolve→false) from message types
		// we silently ignore : d.MAC is set on every non-malformed
		// packet, and MsgType is 0 here. The signal we have without
		// re-parsing is whether the inbound was a DHCP message type
		// we'd otherwise reply to.
		if mt := pkt.MessageType(); mt == MsgDiscover || mt == MsgRequest {
			s.metrics.RecordPacket(OutcomeDropUnknownMAC)
		} else {
			s.metrics.RecordPacket(OutcomeDropUnsupported)
		}
		return
	}
	if err := s.send(conn, pkt, d.Reply); err != nil {
		s.logger.Warn("send", "mac", d.MAC, "msg_type", d.MsgType, "err", err)
		s.metrics.RecordPacket(OutcomeSendErr)
		return
	}
	s.logger.Info("sent", "mac", d.MAC, "msg_type", d.MsgType)
	switch d.MsgType {
	case MsgOffer:
		s.metrics.RecordPacket(OutcomeOffer)
	case MsgAck:
		s.metrics.RecordPacket(OutcomeAck)
	case MsgNak:
		s.metrics.RecordPacket(OutcomeNak)
	}
}

// send writes the reply on UDP/68. v0 strategy : always broadcast to
// 255.255.255.255 (covers every switch, doesn't depend on the client
// already having ARP'd us). SO_BINDTODEVICE ensures the broadcast
// goes out the right NIC even though the destination is global.
func (s *LinuxServer) send(conn *net.UDPConn, _ *Packet, payload []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := conn.WriteToUDP(payload, s.dst)
	return err
}

// ServerIP exposes the configured server identifier (for tests +
// for callers that want to log it once at start-up).
func (s *LinuxServer) ServerIP() netip.Addr { return s.opts.ServerIP }
