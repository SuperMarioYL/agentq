package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
)

// AttachOptions are the flag values for `agentq attach`.
type AttachOptions struct {
	DaemonURL string
	TokenFile string
	Token     string
	Port      int
	IP        string // explicit LAN IP override; skips auto-detection
}

// NewAttachCmd builds the `attach` subcommand.
func NewAttachCmd() *cobra.Command {
	opts := AttachOptions{Port: 7777}
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Print a QR code pointing at the local daemon over LAN.",
		Long: `attach figures out your machine's first non-loopback IPv4 address,
combines it with the daemon token (read from --token, --token-file, or
$AGENTQ_TOKEN), and prints a terminal QR code. Scan with your phone
camera to open the triage queue web UI.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunAttach(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.DaemonURL, "daemon-url", "",
		"override the full target URL (skip auto LAN-IP detection)")
	cmd.Flags().StringVar(&opts.TokenFile, "token-file", "",
		"file to read the daemon token from (e.g. the one --token-out wrote)")
	cmd.Flags().StringVar(&opts.Token, "token", "",
		"explicit token (overrides --token-file and $AGENTQ_TOKEN)")
	cmd.Flags().IntVar(&opts.Port, "port", 7777,
		"daemon port (only used when --daemon-url is empty)")
	cmd.Flags().StringVar(&opts.IP, "ip", "",
		"explicit LAN IP to advertise (skips auto-detection; use when the auto-picked address isn't reachable from your phone)")
	return cmd
}

// RunAttach is the testable attach entrypoint.
func RunAttach(opts AttachOptions, stdout, stderr io.Writer) error {
	token, err := resolveToken(opts)
	if err != nil {
		return err
	}
	target, err := resolveDaemonURL(opts, token)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "scan this with your phone (same Wi-Fi as this machine):")
	fmt.Fprintln(stdout, "  "+target)
	fmt.Fprintln(stdout)
	qrterminal.GenerateWithConfig(target, qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    stdout,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 1,
	})
	return nil
}

func resolveToken(opts AttachOptions) (string, error) {
	if opts.Token != "" {
		return strings.TrimSpace(opts.Token), nil
	}
	if opts.TokenFile != "" {
		raw, err := os.ReadFile(opts.TokenFile)
		if err != nil {
			return "", fmt.Errorf("attach: read token-file %q: %w", opts.TokenFile, err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if env := os.Getenv("AGENTQ_TOKEN"); env != "" {
		return strings.TrimSpace(env), nil
	}
	return "", fmt.Errorf("attach: no token (pass --token, --token-file, or set AGENTQ_TOKEN)")
}

func resolveDaemonURL(opts AttachOptions, token string) (string, error) {
	if opts.DaemonURL != "" {
		u, err := url.Parse(opts.DaemonURL)
		if err != nil {
			return "", fmt.Errorf("attach: parse --daemon-url: %w", err)
		}
		q := u.Query()
		q.Set("t", token)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	ip := strings.TrimSpace(opts.IP)
	if ip == "" {
		var err error
		ip, err = LANIP()
		if err != nil {
			return "", err
		}
	}
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(ip, fmt.Sprintf("%d", opts.Port)),
		Path:     "/",
		RawQuery: "t=" + url.QueryEscape(token),
	}
	return u.String(), nil
}

// virtualIfacePrefixes are interface name prefixes for adapters a phone on
// the same Wi-Fi cannot reach: container bridges, VPN tunnels, virtualization
// host-only nets. Picking one of these is the usual cause of an unscannable QR
// code, so they are considered only as a last resort.
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "utun", "tun", "tap", "vbox",
	"vmnet", "vnic", "ham", "zt", "wg", "tailscale", "llw", "awdl",
}

// LANIP returns the best IPv4 address to advertise to a phone on the same
// Wi-Fi. It prefers a private-range address (192.168/16, 10/8, 172.16/12) on
// a physical-looking interface and only falls back to other non-loopback
// addresses when no private one is found. Container/VPN/virtual interfaces are
// deprioritized because their addresses are typically unreachable from the
// phone — picking the first non-loopback IPv4 (the old behavior) routinely
// returned a Docker bridge or VPN address and broke the QR scan.
func LANIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("attach: list interfaces: %w", err)
	}

	var cands []ifaceCandidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ip := pickV4(a); ip != nil {
				cands = append(cands, ifaceCandidate{name: iface.Name, ip: ip})
			}
		}
	}
	if ip := bestLANIP(cands); ip != nil {
		return ip.String(), nil
	}
	return "", fmt.Errorf("attach: no non-loopback IPv4 interface found (pass --ip to set one explicitly)")
}

// ifaceCandidate is one (interface name, IPv4) pair considered by bestLANIP.
type ifaceCandidate struct {
	name string
	ip   net.IP
}

// bestLANIP chooses the address most likely reachable from a phone on the
// same Wi-Fi: a private-range address on a physical interface wins outright;
// a private address on a virtual interface is second; any other non-loopback
// IPv4 is the last resort. Returns nil when there are no candidates. Pure +
// dependency-free so the selection policy is unit-testable without touching
// real host interfaces.
func bestLANIP(cands []ifaceCandidate) net.IP {
	var privateFallback, otherFallback net.IP
	for _, c := range cands {
		if c.ip == nil {
			continue
		}
		priv := c.ip.IsPrivate()
		virtual := isVirtualIface(c.name)
		switch {
		case priv && !virtual:
			return c.ip
		case priv && privateFallback == nil:
			privateFallback = c.ip
		case otherFallback == nil:
			otherFallback = c.ip
		}
	}
	if privateFallback != nil {
		return privateFallback
	}
	return otherFallback
}

func isVirtualIface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func pickV4(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		if ip4 := v.IP.To4(); ip4 != nil {
			return ip4
		}
	case *net.IPAddr:
		if ip4 := v.IP.To4(); ip4 != nil {
			return ip4
		}
	}
	return nil
}
