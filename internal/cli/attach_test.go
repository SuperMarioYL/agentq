package cli

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestRunAttach_RejectsEmptyToken guards fix-attach-empty-token-silent-broken-qr:
// resolveToken returns ("", nil) for any source whose trimmed value is empty —
// a whitespace-only "--token ' '", an empty "--token-file" (os.ReadFile succeeds
// on a 0-byte file), or a spaces-only $AGENTQ_TOKEN — because no branch checks
// for emptiness after TrimSpace. Without the RunAttach guard the QR is built
// with an empty "?t=" and the daemon 401-rejects every phone request, so the
// operator gets a silently dead QR with no error. The fix makes RunAttach return
// a clear error and print no QR. Each subtest uses --daemon-url so that, were
// the guard absent, RunAttach would proceed to print a QR (making the
// mutation check deterministic rather than depending on LANIP()).
func TestRunAttach_RejectsEmptyToken(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "empty-token")
	if err := os.WriteFile(tokFile, []byte("   \n\t"), 0o600); err != nil {
		t.Fatalf("write token-file: %v", err)
	}

	cases := []struct {
		name string
		opts AttachOptions
		env  string // AGENTQ_TOKEN value (set only when non-empty)
	}{
		{"whitespace-only --token", AttachOptions{Token: "   ", DaemonURL: "http://example.com"}, ""},
		{"empty --token-file", AttachOptions{TokenFile: tokFile, DaemonURL: "http://example.com"}, ""},
		{"whitespace-only $AGENTQ_TOKEN", AttachOptions{DaemonURL: "http://example.com"}, "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("AGENTQ_TOKEN", tc.env)
			} else {
				t.Setenv("AGENTQ_TOKEN", "")
			}
			var out, errOut bytes.Buffer
			err := RunAttach(tc.opts, &out, &errOut)
			if err == nil {
				t.Fatalf("RunAttach returned nil error; want an empty-token error")
			}
			if !strings.Contains(err.Error(), "resolved token is empty") {
				t.Errorf("error=%q; want it to mention \"resolved token is empty\"", err.Error())
			}
			if out.Len() != 0 {
				t.Errorf("stdout was not empty (%d bytes); attach must NOT print a QR for an empty token: %q", out.Len(), out.String())
			}
		})
	}
}

// TestRunAttach_AcceptsNonEmptyToken confirms the empty-token guard does not
// fire for a real (trimmed-non-empty) token: the QR is printed as before. The
// health preflight (m_attach_preflight) is stubbed so the test never touches
// the network; the warning path is covered separately below.
func TestRunAttach_AcceptsNonEmptyToken(t *testing.T) {
	t.Setenv("AGENTQ_TOKEN", "")
	var out, errOut bytes.Buffer
	opts := AttachOptions{
		Token:     "  realtoken  ",
		DaemonURL: "http://example.com",
		Probe:     func(string) error { return nil },
	}
	err := RunAttach(opts, &out, &errOut)
	if err != nil {
		t.Fatalf("RunAttach: %v", err)
	}
	if !strings.Contains(out.String(), "http://example.com") {
		t.Errorf("stdout=%q; want it to contain the daemon URL with the QR", out.String())
	}
	if strings.Contains(errOut.String(), "WARNING") {
		t.Errorf("stderr=%q; want no warning when the health check passes", errOut.String())
	}
}

// TestRunAttach_WarnsWhenDaemonUnreachable guards m_attach_preflight: when
// nothing answers the health check at the resolved target, attach must print
// a loud stderr warning naming the likely causes (including `serve --lan` for
// the loopback bind) and STILL print the QR — a warning, not the hard error
// the empty-token guard raises, because the phone may reach the daemon by a
// path this host cannot.
func TestRunAttach_WarnsWhenDaemonUnreachable(t *testing.T) {
	t.Setenv("AGENTQ_TOKEN", "")
	var out, errOut bytes.Buffer
	opts := AttachOptions{
		Token:     "realtoken",
		DaemonURL: "http://127.0.0.1:7777",
		Probe:     func(string) error { return fmt.Errorf("connection refused") },
	}
	err := RunAttach(opts, &out, &errOut)
	if err != nil {
		t.Fatalf("RunAttach: %v (the preflight is a warning, not an error)", err)
	}
	if !strings.Contains(errOut.String(), "WARNING") {
		t.Errorf("stderr=%q; want the unreachable-daemon warning", errOut.String())
	}
	if !strings.Contains(errOut.String(), "--lan") {
		t.Errorf("stderr=%q; want the warning to name `serve --lan` as a cause", errOut.String())
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:7777") {
		t.Errorf("stdout=%q; want the full target still printed", out.String())
	}
	if !strings.Contains(out.String(), "scan this with your phone") {
		t.Errorf("stdout=%q; want the QR block still printed", out.String())
	}
}

// TestProbeDaemonHealth_StripsTokenAndPathsToHealthz checks the real probe
// helper hits exactly /healthz with no query — the bearer token must not ride
// along on the preflight request.
func TestProbeDaemonHealth_StripsTokenAndPathsToHealthz(t *testing.T) {
	var seenPath, seenRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := srv.URL + "/?t=secrettoken"
	if err := probeDaemonHealth(target); err != nil {
		t.Fatalf("probeDaemonHealth: %v", err)
	}
	if seenPath != "/healthz" {
		t.Errorf("probe hit %q; want /healthz", seenPath)
	}
	if seenRawQuery != "" {
		t.Errorf("probe carried query %q; want the bearer token stripped", seenRawQuery)
	}
}

// TestProbeDaemonHealth_Non2xxIsUnreachable: a 404 from something that is not
// an agentq daemon must count as "unreachable" so the warning still fires.
func TestProbeDaemonHealth_Non2xxIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not the daemon", http.StatusNotFound)
	}))
	defer srv.Close()

	if err := probeDaemonHealth(srv.URL); err == nil {
		t.Fatalf("probeDaemonHealth: want an error for a non-2xx /healthz response")
	}
}
