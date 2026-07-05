package wrapper

import (
	"context"
	"io"
)

// childProcess is the platform-independent handle the wrapper drives a coding
// agent through: three stdio streams plus lifecycle control. It abstracts over
// HOW the child is spawned so the wrapper's IO loop (Process) is identical on
// every OS.
//
// The concrete implementation is build-tagged:
//
//	spawn_unix.go     (//go:build !windows) — os/exec pipes; the original macOS
//	                  + Linux path, byte-for-byte unchanged behavior.
//	spawn_windows.go  (//go:build windows)  — a pipe-based stdio fallback, since
//	                  Windows has no Unix pty. Same wire format, same daemon/UI.
//
// Splitting spawn per-OS keeps Windows support additive: the shared abstraction
// and the wrapper loop have one code path, only the process-launch differs.
type childProcess interface {
	// Stdin is the writer the wrapper forwards answer keys to.
	Stdin() io.Writer
	// Stdout is the reader the wrapper scans for permission prompts.
	Stdout() io.Reader
	// Stderr is the reader mirrored to the operator's terminal.
	Stderr() io.Reader
	// Start launches the process.
	Start() error
	// Wait blocks until the process exits and returns its exit error.
	Wait() error
	// CloseStdin closes the child's stdin (EOF), signalling no more input.
	CloseStdin() error
}

// spawnChild is set by the build-tagged spawn_{unix,windows}.go to the
// OS-appropriate constructor. It builds (but does not Start) a childProcess for
// the given command. ctx is used for cancellation-driven kill, matching
// exec.CommandContext semantics on both platforms.
var spawnChild func(ctx context.Context, name string, args ...string) (childProcess, error)
