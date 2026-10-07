<p align="center"><img src="https://raw.githubusercontent.com/go-net-dhcp/brand/main/social/go-net-dhcp-dhcp.png" alt="go-net-dhcp/dhcp" width="720"></p>

# dhcp — go-net-dhcp

[![Docs](https://img.shields.io/badge/docs-mkdocs--material-2563EB)](https://go-net-dhcp.github.io/docs/)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27.1%2B-00ADD8)](https://go.dev/dl/)
[![Coverage](https://img.shields.io/badge/coverage-100%25-1a7f37)](#tests--coverage)

**A small, dependency-free DHCPv4 server library in pure Go (no cgo).** It
implements the RFC 2131 / 2132 wire format (parse a DISCOVER/REQUEST, build an
OFFER/ACK/NAK), a stateless platform-agnostic decision core, and a real Linux
UDP/67 server bound to a single kernel interface via `SO_BINDTODEVICE`.

It is built for the *"one subnet, hand out a lease per known MAC"* case — a host
that owns a bridge / VLAN and needs to answer DHCP for the guests it spawns on
it, **without running an external dnsmasq**. You supply a `Source` that resolves
a client MAC into a `Lease`; the library never persists anything itself.

The DHCPv4 wire codec is hand-rolled (a fixed BOOTP header + a 4-byte magic
cookie + a block of TLV options terminated by code 255) so the module has **zero
third-party dependencies** in its core. Metrics are optional and the Prometheus
adapter lives in a separate sub-package.

## Features

- **RFC 2131 / 2132 wire codec** — `Parse` and `BuildReply` (OFFER / ACK / NAK),
  strict on framing, tolerant of unknown option codes.
- **Stateless decision core** — `Decide(pkt, opts)` runs the server state machine
  (DISCOVER→OFFER, REQUEST→ACK, mismatched requested-IP→NAK, unknown MAC / other
  message types→drop). Build-tag-free, so it is tested on every platform.
- **Real Linux server** — `LinuxServer` binds UDP/67 to one interface via
  `SO_BINDTODEVICE` + `SO_BROADCAST` + `SO_REUSEADDR` and replies by broadcast.
- **Cross-platform surface** — a build stub on non-Linux plus a `StubServer` that
  drives the `Source` pipeline with no socket, so callers compile and test
  everywhere.
- **Optional metrics** — pass any `Metrics` implementation in `Options`; a
  ready-made Prometheus adapter is in
  [`github.com/go-net-dhcp/dhcp/prom`](./prom), keeping the core dependency-free.

CGO-free, **100% test coverage** (error branches included, socket paths covered
via injectable syscall seams — no root needed), `gofmt` + `go vet` clean, and
green across the six 64-bit Go targets (amd64, arm64, riscv64, loong64, ppc64le,
s390x).

## Install

```sh
go get github.com/go-net-dhcp/dhcp
```

## Usage

```go
package main

import (
	"context"
	"net/netip"

	"github.com/go-net-dhcp/dhcp"
)

func main() {
	opts := dhcp.Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source: dhcp.SourceFn(func(mac string) (dhcp.Lease, bool) {
			if mac != "52:54:00:00:00:01" {
				return dhcp.Lease{}, false // unknown MAC → silently dropped
			}
			return dhcp.Lease{
				Yiaddr:         netip.MustParseAddr("10.0.0.42"),
				SubnetMaskBits: 24,
				Router:         netip.MustParseAddr("10.0.0.1"),
				DNSServers:     []netip.Addr{netip.MustParseAddr("9.9.9.9")},
			}, true
		}),
	}

	srv, err := dhcp.NewLinuxServer(opts)
	if err != nil {
		panic(err)
	}
	// Run blocks until ctx is cancelled (or the socket errors out).
	_ = srv.Run(context.Background())
}
```

The wire codec and decision core are usable on their own:

```go
pkt, _ := dhcp.Parse(raw)                    // decode an inbound packet
d, _ := dhcp.Decide(pkt, opts)               // OFFER / ACK / NAK / drop
// d.Reply is the bytes to send (nil = drop this packet)
```

### Metrics (optional)

```go
import (
	"github.com/go-net-dhcp/dhcp"
	dhcpprom "github.com/go-net-dhcp/dhcp/prom"
	"github.com/prometheus/client_golang/prometheus"
)

m, _ := dhcpprom.New(prometheus.DefaultRegisterer)
opts.Metrics = m // dhcpv4_packets_total{outcome} + dhcpv4_handle_duration_seconds
```

## API

```go
type Options struct {
	Interface string      // kernel interface to bind (SO_BINDTODEVICE)
	ServerIP  netip.Addr  // announced as option 54 (server identifier)
	Source    Source      // resolves a client MAC → Lease
	Metrics   Metrics     // optional per-packet telemetry sink
}

type Lease struct {
	Yiaddr         netip.Addr    // handed to the client
	SubnetMaskBits int           // prefix length → option 1
	Router         netip.Addr    // option 3 (optional)
	DNSServers     []netip.Addr  // option 6 (optional)
	Domain         string        // option 15 (optional)
	LeaseTime      time.Duration // option 51 (0 = 1h default)
}

type Source interface{ Resolve(mac string) (Lease, bool) }

func Parse(buf []byte) (*Packet, error)
func BuildReply(req *Packet, msgType byte, serverID netip.Addr, lease Lease) ([]byte, error)
func Decide(pkt *Packet, opts Options) (Decision, error)

func NewLinuxServer(opts Options) (*LinuxServer, error) // real UDP/67 server (linux)
func NewStub(opts Options) (*StubServer, error)         // socket-free, every platform
```

## Tests & coverage

```sh
COVERPKG=$(go list ./... | paste -sd, -)
go test -race -coverpkg="$COVERPKG" -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1   # 100.0%
```

The Linux socket path is covered without root by injecting the socket / setsockopt
/ bind / fd-adoption calls behind a seam, so every error branch (and the full
receive → decide → send loop) is exercised in CI on the ubuntu lane.

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright the go-net-dhcp/dhcp authors.
