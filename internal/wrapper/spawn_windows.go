//go:build windows

package wrapper

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// pipeChild is the Windows child-process handle. Windows has no Unix pty, so the
// wrapper drives the agent over plain os/exec stdin/stdout/stderr pipes — a
// line-buffered stdio fallback. The agent still writes its permission prompts to
// stdout and reads the chosen key from stdin, so the ApprovalEnvelope wire
// format and the daemon/UI are unchanged; only the launch mechanism differs from
// the Unix pty-style path. A future ConPTY-backed implementation can replace
// this constructor without touching the wrapper's IO loop.
type pipeChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func init() {
	spawnChild = newPipeChild
}

func newPipeChild(ctx context.Context, name string, args ...string) (childProcess, error) {
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
	return &pipeChild{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (c *pipeChild) Stdin() io.Writer  { return c.stdin }
func (c *pipeChild) Stdout() io.Reader { return c.stdout }
func (c *pipeChild) Stderr() io.Reader { return c.stderr }
func (c *pipeChild) Start() error      { return c.cmd.Start() }
func (c *pipeChild) Wait() error       { return c.cmd.Wait() }
func (c *pipeChild) CloseStdin() error { return c.stdin.Close() }
