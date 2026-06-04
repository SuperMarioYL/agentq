package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	agentq "github.com/SuperMarioYL/agentq"
	"github.com/SuperMarioYL/agentq/internal/daemon"
)

func init() {
	// Register the embedded SPA so the daemon serves it when this
	// CLI binary runs. Tests that import only internal/daemon leave
	// webAssets nil and the / handler responds with a placeholder.
	daemon.SetWebAssets(agentq.WebFS)
}

// ServeOptions are the flag values for `agentq serve`.
type ServeOptions struct {
	Listen   string
	DataDir  string
	Token    string
	TokenOut string
	LAN      bool
}

// NewServeCmd builds the `serve` subcommand.
func NewServeCmd() *cobra.Command {
	opts := ServeOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the local daemon that aggregates approval prompts.",
		Long: `serve runs the agentq daemon — a single Go process bound to a
local address (default 127.0.0.1:7777) that receives ApprovalEnvelopes
from N wrapped agent sessions, stores them in a bbolt file under
--data-dir, and serves the embedded triage SPA + REST + WebSocket.

On first run a bearer token is generated and printed once. Use --token
to override (e.g. when re-binding the same DB after restart). Add --lan
to bind 0.0.0.0 and accept connections from your phone.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunServe(cmd.Context(), opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.Listen, "listen", "127.0.0.1:7777",
		"host:port to bind the daemon to (use --lan to bind 0.0.0.0)")
	cmd.Flags().StringVar(&opts.DataDir, "data-dir", "",
		"directory for the bbolt store (default: $XDG_DATA_HOME/agentq or ~/.agentq)")
	cmd.Flags().StringVar(&opts.Token, "token", "",
		"bearer token clients must present (default: generate one)")
	cmd.Flags().StringVar(&opts.TokenOut, "token-out", "",
		"optional file to write the active token to (consumed by `agentq attach`)")
	cmd.Flags().BoolVar(&opts.LAN, "lan", false,
		"shorthand to bind 0.0.0.0 so phones can reach the daemon over LAN")
	return cmd
}

// RunServe is the testable serve entrypoint.
func RunServe(parent context.Context, opts ServeOptions, stdout, stderr io.Writer) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if opts.LAN {
		opts.Listen = forceBindAll(opts.Listen)
	}

	dir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("serve: create data dir %q: %w", dir, err)
	}
	dbPath := filepath.Join(dir, "queue.db")
	store, err := daemon.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	token := opts.Token
	if token == "" {
		token, err = randomToken()
		if err != nil {
			return fmt.Errorf("serve: generate token: %w", err)
		}
	}
	if opts.TokenOut != "" {
		if err := os.WriteFile(opts.TokenOut, []byte(token), 0o600); err != nil {
			return fmt.Errorf("serve: write token-out %q: %w", opts.TokenOut, err)
		}
	}

	cfg := daemon.Config{
		Listen: opts.Listen,
		Token:  token,
		Store:  store,
		Queue:  daemon.NewQueue(),
	}
	srv := daemon.NewServer(cfg)

	fmt.Fprintf(stdout, "agentq daemon listening on http://%s\n", opts.Listen)
	fmt.Fprintf(stdout, "data-dir : %s\n", dir)
	fmt.Fprintf(stdout, "token    : %s\n", token)
	if opts.TokenOut != "" {
		fmt.Fprintf(stdout, "token written to %s — run `agentq attach --token-file %s`.\n",
			opts.TokenOut, opts.TokenOut)
	} else {
		fmt.Fprintf(stdout, "run `agentq attach --token %s` to get a QR code for your phone.\n", token)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()
	select {
	case <-ctx.Done():
		select {
		case err := <-runErr:
			return err
		case <-time.After(6 * time.Second):
			return errors.New("serve: shutdown timed out")
		}
	case err := <-runErr:
		return err
	}
}

func resolveDataDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "agentq"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("serve: locate home dir: %w", err)
	}
	return filepath.Join(home, ".agentq"), nil
}

func forceBindAll(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "0.0.0.0:7777"
	}
	return net.JoinHostPort("0.0.0.0", port)
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
