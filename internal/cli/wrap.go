// Package cli holds the cobra commands wired up by cmd/agentq. Each
// subcommand keeps its own file so the entrypoint stays a thin shell.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SuperMarioYL/agentq/internal/protocol"
	"github.com/SuperMarioYL/agentq/internal/wrapper"
)

// WrapOptions are the flag values for `agentq wrap`. Exported so tests
// (and a future programmatic embed) can drive runWrap directly.
type WrapOptions struct {
	AgentID     string
	EnvelopeOut string
	AnswerIn    string
	Expiry      time.Duration
	// Agent selects the prompt dialect to recognize: "claude" (default,
	// bracketed "[y/n]" form), "cursor"/"aider" (parenthesized "(Y)es/(N)o"
	// form), or "auto" (try both). All emit the same ApprovalEnvelope.
	Agent string

	// Daemon, when true, starts (or reuses) the local serve daemon and forwards
	// this agent's envelopes to it — one command instead of `serve` + `wrap`.
	Daemon bool
	// DaemonListen is the daemon host:port used in --daemon mode. Empty defaults
	// to 127.0.0.1:7777 (the serve default).
	DaemonListen string
	// DaemonToken is an explicit bearer token for --daemon mode. Empty means a
	// reused daemon needs no token from us, or a freshly-started one mints its own.
	DaemonToken string
}

// matchersForAgent maps the --agent value to the PromptMatcher set the
// wrapper should run. Unknown values fall back to "auto" so a typo never
// silently drops all prompts. "auto" runs the Claude matcher first (it
// requires a slash, so it won't steal Cursor/Aider lines) then the Cursor one.
func matchersForAgent(agent string) []wrapper.PromptMatcher {
	switch wrapper.NormalizeAgent(agent) {
	case "claude":
		return []wrapper.PromptMatcher{wrapper.DefaultMatcher}
	case "cursor", "aider":
		return []wrapper.PromptMatcher{wrapper.CursorMatcher}
	default: // "auto"
		return []wrapper.PromptMatcher{wrapper.DefaultMatcher, wrapper.CursorMatcher}
	}
}

// NewWrapCmd builds the `wrap` subcommand. In m1 the wrapper has no
// daemon: --envelope-out defaults to stdout and --answer-in defaults to
// stdin, so a second terminal can tail and reply via plain pipes.
func NewWrapCmd() *cobra.Command {
	opts := WrapOptions{Expiry: protocol.DefaultExpiry}
	cmd := &cobra.Command{
		Use:   "wrap [flags] -- <command> [args...]",
		Short: "Run a coding agent and intercept its permission prompts.",
		Long: `wrap launches the given command, watches its stdout for permission
prompts, and emits one ApprovalEnvelope (newline-delimited JSON) per
prompt to --envelope-out. It then blocks until a matching Answer JSON
arrives on --answer-in, at which point the chosen key is written to the
agent's stdin.

The m1 default routes envelopes to stdout and reads answers from stdin,
so the loop can be driven with shell redirection or two terminals. The
m2 daemon will speak the same wire format over HTTP+WebSocket.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunWrap(cmd.Context(), opts, args, cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin())
		},
	}
	cmd.Flags().StringVar(&opts.AgentID, "agent-id", "",
		"Label for this session (default: derived from command name).")
	cmd.Flags().StringVar(&opts.EnvelopeOut, "envelope-out", "",
		"File to write ApprovalEnvelope JSON to. Empty = stdout.")
	cmd.Flags().StringVar(&opts.AnswerIn, "answer-in", "",
		"File to read Answer JSON from. Empty = stdin.")
	cmd.Flags().DurationVar(&opts.Expiry, "expiry", protocol.DefaultExpiry,
		"How long each envelope is valid without a reply.")
	cmd.Flags().StringVar(&opts.Agent, "agent", "auto",
		"Prompt dialect to recognize: claude (bracketed [y/n]), cursor/aider (parenthesized (Y)es/(N)o), or auto (both).")
	cmd.Flags().BoolVar(&opts.Daemon, "daemon", false,
		"Start (or reuse) the local serve daemon and forward this agent's prompts to it, so no separate `agentq serve` is needed.")
	cmd.Flags().StringVar(&opts.DaemonListen, "daemon-listen", "127.0.0.1:7777",
		"host:port of the serve daemon to reuse or start in --daemon mode.")
	cmd.Flags().StringVar(&opts.DaemonToken, "daemon-token", "",
		"bearer token for --daemon mode (default: reuse needs none / a started daemon mints one).")
	return cmd
}

// RunWrap is the testable wrap entrypoint: build a Wrapper from opts,
// translate the envelope-out / answer-in paths into io.Writer/Reader,
// then drive Wrapper.Run.
func RunWrap(parent context.Context, opts WrapOptions, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("wrap: missing command (use -- before the agent invocation)")
	}
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var (
		envOut io.Writer
		ansIn  io.Reader
	)

	if opts.Daemon {
		// --daemon: bootstrap (reuse-or-start) the local serve daemon, then route
		// this agent's envelopes to it over HTTP instead of files/stdio.
		fwd, closeFwd, derr := setupDaemonForward(ctx, opts, stdout)
		if derr != nil {
			return derr
		}
		defer closeFwd()
		envOut = fwd
		ansIn = fwd
	} else {
		eo, closeEnv, err := openEnvelopeOut(opts.EnvelopeOut, stdout)
		if err != nil {
			return fmt.Errorf("wrap: --envelope-out %q: %w", opts.EnvelopeOut, err)
		}
		defer closeEnv()

		ai, closeAns, err := openAnswerIn(opts.AnswerIn, stdin)
		if err != nil {
			return fmt.Errorf("wrap: --answer-in %q: %w", opts.AnswerIn, err)
		}
		defer closeAns()
		envOut = eo
		ansIn = ai
	}

	w := &wrapper.Wrapper{
		Cmd:         args,
		AgentID:     opts.AgentID,
		Matchers:    matchersForAgent(opts.Agent),
		EnvelopeOut: envOut,
		AnswerIn:    ansIn,
		Stdout:      stdout,
		Stderr:      stderr,
		Expiry:      opts.Expiry,
	}
	err := w.Run(ctx)
	if err != nil && ctx.Err() != nil {
		// Treat interrupt-driven shutdown as success — the operator asked for it.
		return nil
	}
	return err
}

// setupDaemonForward implements `wrap --daemon`: ensure a serve daemon is up
// (reuse the one already on the port, else start one in-process), then return a
// daemonForwarder wired to it as both the wrapper's envelope sink and answer
// source. The returned close func flushes the forwarder and stops any daemon
// this wrap started (reused daemons are left running).
func setupDaemonForward(ctx context.Context, opts WrapOptions, stdout io.Writer) (*daemonForwarder, func(), error) {
	listen := opts.DaemonListen
	if listen == "" {
		listen = "127.0.0.1:7777"
	}
	boot := daemonBootstrap{
		Listen: listen,
		Token:  opts.DaemonToken,
		probe:  probeDaemon,
		start:  startInProcessDaemon(stdout),
	}
	handle, err := boot.ensureDaemon(ctx)
	if err != nil {
		return nil, nil, err
	}
	if handle.Reused {
		fmt.Fprintf(stdout, "wrap --daemon: reusing daemon at %s\n", handle.BaseURL)
	} else {
		fmt.Fprintf(stdout, "wrap --daemon: started daemon at %s\n", handle.BaseURL)
	}

	fwd := newDaemonForwarder(handle.BaseURL, handle.Token)
	closeFn := func() {
		_ = fwd.Close()
		handle.Close()
	}
	return fwd, closeFn, nil
}

// startInProcessDaemon returns a daemonBootstrap.start that launches the serve
// daemon inside this process (a goroutine running RunServe) and blocks until it
// is accepting connections. The returned stop func cancels it and waits briefly
// for a clean shutdown.
func startInProcessDaemon(stdout io.Writer) func(ctx context.Context, listen, token string) (func(), error) {
	return func(ctx context.Context, listen, token string) (func(), error) {
		serveCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = RunServe(serveCtx, ServeOptions{
				Listen: listen,
				Token:  token,
			}, io.Discard, io.Discard)
		}()
		if err := waitForDaemon(listen, 5*time.Second); err != nil {
			cancel()
			<-done
			return nil, err
		}
		stop := func() {
			cancel()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
			}
		}
		return stop, nil
	}
}

func openEnvelopeOut(path string, fallback io.Writer) (io.Writer, func() error, error) {
	if path == "" {
		return fallback, noopClose, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

func openAnswerIn(path string, fallback io.Reader) (io.Reader, func() error, error) {
	if path == "" {
		return fallback, noopClose, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

func noopClose() error { return nil }
