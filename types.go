// Package dhcp is a small, dependency-free DHCPv4 server library in
// pure Go (no cgo). It implements the RFC 2131 / 2132 wire codec
// (parse + build OFFER/ACK/NAK) and a stateless, platform-agnostic
// decision core, plus a real Linux UDP/67 server bound to a single
// kernel interface via SO_BINDTODEVICE.
//
// It is designed for the "one subnet, hand out leases per known MAC"
// case — a host that owns a bridge / VLAN and needs to answer DHCP
// for the guests it spawns on it, without pulling in an external
// dnsmasq. The caller supplies a Source that resolves a client MAC
// into a Lease ; the server never persists anything itself.
//
// Architecture:
//
//	Source ─ Resolve(mac) → Lease{Yiaddr, Router, DNS, ...}
//	                ▼
//	Server (LinuxServer) listens on the bound interface UDP/67,
//	       parses DISCOVER/REQUEST, runs Decide, and replies via
//	       broadcast to the client.
//
// The wire codec (Parse / BuildReply) and the Decide state machine
// are build-tag-free and portable ; only the socket wiring is
// Linux-only (server_linux.go). Non-Linux platforms get a build stub
// plus StubServer for exercising the Source pipeline in tests.
//
// Metrics are optional: pass a Metrics implementation in Options to
// receive per-packet telemetry. A ready-made Prometheus adapter
// lives in the sibling package github.com/go-net-dhcp/dhcp/prom so
// the core library keeps zero third-party dependencies.
package dhcp

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// Lease is the per-MAC response the server hands out. Fields
// mirror the standard DHCPv4 options the server emits :
//
//   - Yiaddr      → option type 1 (subnet mask is derived from
//                   the Lease's prefix length)
//   - Router      → option 3
//   - DNSServers  → option 6
//   - Domain      → option 15 (optional)
//   - LeaseTime   → option 51 (defaults to 1h when zero)
//
// The lease is computed by the caller's Source on each request ;
// the server doesn't persist anything itself.
type Lease struct {
	// Yiaddr is the address handed to the client.
	Yiaddr netip.Addr
	// SubnetMaskBits is the prefix length (e.g. 24 for /24)
	// from which the server derives the netmask option.
	SubnetMaskBits int
	// Router is the default gateway. Optional ; zero value
	// skips option 3.
	Router netip.Addr
	// DNSServers is the list of resolvers. Empty skips option 6.
	DNSServers []netip.Addr
	// Domain is the search domain. Empty skips option 15.
	Domain string
	// LeaseTime is the lease validity. Zero = 1h default.
	LeaseTime time.Duration
}

// Validate enforces the minimum invariants the server needs to
// build a well-formed reply.
func (l Lease) Validate() error {
	if !l.Yiaddr.IsValid() {
		return errors.New("lease: yiaddr is required")
	}
	if !l.Yiaddr.Is4() {
		return errors.New("lease: yiaddr must be IPv4 (this is a DHCPv4 server)")
	}
	if l.SubnetMaskBits <= 0 || l.SubnetMaskBits > 32 {
		return fmt.Errorf("lease: subnet_mask_bits out of range (1-32): %d", l.SubnetMaskBits)
	}
	if l.Router.IsValid() && !l.Router.Is4() {
		return errors.New("lease: router must be IPv4")
	}
	for i, ns := range l.DNSServers {
		if !ns.IsValid() || !ns.Is4() {
			return fmt.Errorf("lease: dns[%d] must be a valid IPv4", i)
		}
	}
	if l.LeaseTime < 0 {
		return fmt.Errorf("lease: lease_time must be ≥ 0 : %s", l.LeaseTime)
	}
	return nil
}

// Source resolves a client MAC into a Lease. Returns
// (Lease{}, false) when no lease should be issued (unknown MAC
// → silently dropped, no NAK).
//
// Implementations bridge the server to the caller's own registry:
// look up the client by MAC and build the Lease from whatever
// address / gateway / DNS configuration it owns.
type Source interface {
	Resolve(mac string) (Lease, bool)
}

// SourceFn is a function-type Source for tests + small adapters.
type SourceFn func(mac string) (Lease, bool)

// Resolve calls the function.
func (f SourceFn) Resolve(mac string) (Lease, bool) { return f(mac) }

// Server is the public surface. Run blocks until ctx is cancelled.
// The concrete implementation lives in server_linux.go (real
// UDP/67 + protocol) and server_other.go (no-op stub).
type Server interface {
	Run(ctx contextLike) error
}

// contextLike is the narrow context surface the Server uses.
// Pulled into a tiny interface so test stubs don't need to
// import context (and so this types file stays import-light).
type contextLike interface {
	Done() <-chan struct{}
	Err() error
}

// Options is the constructor input for both real + stub servers.
type Options struct {
	// Interface is the host-side kernel interface the server
	// binds to (e.g. "br0", "eth0.100"). Required ; the server
	// uses SO_BINDTODEVICE so it only sees broadcast traffic
	// from this VLAN / bridge.
	Interface string
	// ServerIP is the address the server announces as option 54
	// (server identifier). Conventionally the host's address
	// on the served network. Required.
	ServerIP netip.Addr
	// Source resolves the client MAC into a Lease. Required.
	Source Source
	// Metrics receives per-packet telemetry. Optional ; a nil
	// Metrics discards everything. Use the prom subpackage for a
	// Prometheus adapter without pulling that dependency into the
	// core library.
	Metrics Metrics
}

// Validate checks the cross-field invariants.
func (o Options) Validate() error {
	if o.Interface == "" {
		return errors.New("dhcp: empty Interface")
	}
	if !o.ServerIP.IsValid() || !o.ServerIP.Is4() {
		return errors.New("dhcp: ServerIP must be a valid IPv4")
	}
	if o.Source == nil {
		return errors.New("dhcp: nil Source")
	}
	return nil
}
