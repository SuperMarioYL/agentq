package cli

import (
	"net"
	"strings"
	"testing"
)

// TestBestLANIP_PrefersPhysicalPrivate guards the QR-reachability fix: when a
// Docker bridge (172.17.x on "docker0") is enumerated before the Wi-Fi NIC
// (192.168.x on "en0"), the picker must NOT return the bridge address — the
// phone can't reach it. The old first-non-loopback logic returned docker0.
func TestBestLANIP_PrefersPhysicalPrivate(t *testing.T) {
	cands := []ifaceCandidate{
		{name: "docker0", ip: net.ParseIP("172.17.0.1")},
		{name: "en0", ip: net.ParseIP("192.168.1.42")},
	}
	got := bestLANIP(cands)
	if got == nil || got.String() != "192.168.1.42" {
		t.Fatalf("got %v want 192.168.1.42 (physical private over docker bridge)", got)
	}
}

func TestBestLANIP_SkipsVPNTunnel(t *testing.T) {
	cands := []ifaceCandidate{
		{name: "utun3", ip: net.ParseIP("10.8.0.6")},   // VPN tunnel
		{name: "en0", ip: net.ParseIP("192.168.0.10")}, // Wi-Fi
	}
	got := bestLANIP(cands)
	if got == nil || got.String() != "192.168.0.10" {
		t.Fatalf("got %v want 192.168.0.10 (physical over VPN tunnel)", got)
	}
}

func TestBestLANIP_PrivateVirtualBeatsPublic(t *testing.T) {
	// Only a virtual-iface private addr and a public addr exist: prefer the
	// private one (more likely a LAN the phone shares) over the public.
	cands := []ifaceCandidate{
		{name: "br-abc", ip: net.ParseIP("10.0.0.5")},
		{name: "eth1", ip: net.ParseIP("203.0.113.7")},
	}
	got := bestLANIP(cands)
	if got == nil || got.String() != "10.0.0.5" {
		t.Fatalf("got %v want 10.0.0.5 (private virtual over public)", got)
	}
}

func TestBestLANIP_FallsBackToOther(t *testing.T) {
	cands := []ifaceCandidate{
		{name: "eth0", ip: net.ParseIP("203.0.113.7")},
	}
	got := bestLANIP(cands)
	if got == nil || got.String() != "203.0.113.7" {
		t.Fatalf("got %v want 203.0.113.7 (sole non-loopback)", got)
	}
}

func TestBestLANIP_Empty(t *testing.T) {
	if got := bestLANIP(nil); got != nil {
		t.Fatalf("got %v want nil for no candidates", got)
	}
}

func TestIsVirtualIface(t *testing.T) {
	virtual := []string{"docker0", "br-12ab", "utun0", "tun5", "vboxnet0", "tailscale0", "veth9a", "awdl0"}
	for _, n := range virtual {
		if !isVirtualIface(n) {
			t.Errorf("isVirtualIface(%q)=false want true", n)
		}
	}
	physical := []string{"en0", "eth0", "wlan0", "enp3s0"}
	for _, n := range physical {
		if isVirtualIface(n) {
			t.Errorf("isVirtualIface(%q)=true want false", n)
		}
	}
}

// TestResolveDaemonURL_IPOverride confirms --ip skips auto-detection and is
// used verbatim as the QR host, the explicit escape hatch for the rare case
// the heuristic still picks wrong.
func TestResolveDaemonURL_IPOverride(t *testing.T) {
	got, err := resolveDaemonURL(AttachOptions{IP: "192.168.5.20", Port: 7777}, "tok123")
	if err != nil {
		t.Fatalf("resolveDaemonURL: %v", err)
	}
	if !strings.Contains(got, "192.168.5.20:7777") {
		t.Fatalf("url %q missing override host 192.168.5.20:7777", got)
	}
	if !strings.Contains(got, "t=tok123") {
		t.Fatalf("url %q missing token", got)
	}
}
