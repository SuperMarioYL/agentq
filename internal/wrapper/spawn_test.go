package wrapper

import (
	"bufio"
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSpawnChildRegistered guards m6_windows_support: a platform-appropriate
// child-process launcher must be registered on EVERY OS the wrapper builds for.
// The build-tagged spawn_{unix,windows}.go files set spawnChild in init(); if a
// platform lacked one, Run would fail with "no child-process launcher". This
// test compiles and runs on the host OS (unix here, and the equivalent on
// Windows) and exercises the shared childProcess abstraction end to end.
func TestSpawnChildRegistered(t *testing.T) {
	if spawnChild == nil {
		t.Fatalf("spawnChild is nil on %s — build-tagged launcher not registered", runtime.GOOS)
	}
}

// TestChildProcess_StdioWiring drives the shared childProcess contract: the
// launcher must give back a process whose stdout carries what the child prints
// and whose stdin the child can read. Kept OS-agnostic by shelling out through
// the platform's own interpreter so the SAME test validates the unix path here
// and the windows path there.
func TestChildProcess_StdioWiring(t *testing.T) {
	if spawnChild == nil {
		t.Skip("no launcher registered")
	}
	name, args := echoCmd("hello-from-child")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	child, err := spawnChild(ctx, name, args...)
	if err != nil {
		t.Fatalf("spawnChild: %v", err)
	}
	// Confirm the abstraction hands back all three streams and control methods.
	if child.Stdin() == nil || child.Stdout() == nil || child.Stderr() == nil {
		t.Fatal("childProcess returned a nil stdio stream")
	}

	out := bufio.NewScanner(child.Stdout())
	if err := child.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Child needs no input; close stdin so it never blocks.
	_ = child.CloseStdin()

	var got string
	if out.Scan() {
		got = strings.TrimSpace(out.Text())
	}
	// Drain the rest so Wait doesn't deadlock on a full pipe.
	_, _ = io.Copy(io.Discard, child.Stdout())
	if err := child.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got != "hello-from-child" {
		t.Errorf("child stdout=%q want %q", got, "hello-from-child")
	}
}

// echoCmd returns a command that prints s and exits, using the host OS's shell.
func echoCmd(s string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "echo " + s}
	}
	return "sh", []string{"-c", "echo " + s}
}
