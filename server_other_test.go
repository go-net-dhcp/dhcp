//go:build !linux

package dhcp

import (
	"net/netip"
	"testing"
)

// On non-linux platforms NewLinuxServer validates Options but then
// refuses to build a real server.
func TestLinuxServer_StubOnNonLinux(t *testing.T) {
	// Bad options → validation error.
	if _, err := NewLinuxServer(Options{}); err == nil {
		t.Fatal("expected validation error for empty Options")
	}

	// Good options → linux-only error.
	opts := Options{
		Interface: "br0",
		ServerIP:  netip.MustParseAddr("10.0.0.1"),
		Source:    SourceFn(func(string) (Lease, bool) { return Lease{}, false }),
	}
	s, err := NewLinuxServer(opts)
	if err == nil {
		t.Fatal("expected linux-only error off Linux")
	}
	if s != nil {
		t.Fatalf("expected nil server, got %+v", s)
	}

	// The stub type still carries the method set; exercise it via a
	// zero value so the symbols are covered on this platform.
	var stub LinuxServer
	stub.opts = opts
	stub.SetLogger(nil)
	if err := stub.Run(nil); err == nil {
		t.Error("stub Run should return linux-only error")
	}
	if got := stub.ServerIP(); got != opts.ServerIP {
		t.Errorf("ServerIP = %v, want %v", got, opts.ServerIP)
	}
}
