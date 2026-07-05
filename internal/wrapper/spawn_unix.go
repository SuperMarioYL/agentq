//go:build !windows

package wrapper

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// execChild is the macOS + Linux child-process handle. It drives the agent via
// os/exec's stdin/stdout/stderr pipes — the exact wiring the wrapper has used
// since m1, extracted here behind childProcess so the Windows build can supply
// its own path without touching the IO loop.
type execChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func init() {
	spawnChild = newExecChild
}

func newExecChild(ctx context.Context, name string, args ...string) (childProcess, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("wrapper: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("wrapper: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("wrapper: stderr pipe: %w", err)
	}
	return &execChild{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (c *execChild) Stdin() io.Writer  { return c.stdin }
func (c *execChild) Stdout() io.Reader { return c.stdout }
func (c *execChild) Stderr() io.Reader { return c.stderr }
func (c *execChild) Start() error      { return c.cmd.Start() }
func (c *execChild) Wait() error       { return c.cmd.Wait() }
func (c *execChild) CloseStdin() error { return c.stdin.Close() }
