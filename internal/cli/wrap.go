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

	envOut, closeEnv, err := openEnvelopeOut(opts.EnvelopeOut, stdout)
	if err != nil {
		return fmt.Errorf("wrap: --envelope-out %q: %w", opts.EnvelopeOut, err)
	}
	defer closeEnv()

	ansIn, closeAns, err := openAnswerIn(opts.AnswerIn, stdin)
	if err != nil {
		return fmt.Errorf("wrap: --answer-in %q: %w", opts.AnswerIn, err)
	}
	defer closeAns()

	w := &wrapper.Wrapper{
		Cmd:         args,
		AgentID:     opts.AgentID,
		EnvelopeOut: envOut,
		AnswerIn:    ansIn,
		Stdout:      stdout,
		Stderr:      stderr,
		Expiry:      opts.Expiry,
	}
	err = w.Run(ctx)
	if err != nil && ctx.Err() != nil {
		// Treat interrupt-driven shutdown as success — the operator asked for it.
		return nil
	}
	return err
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
