// Command agentq is the CLI entrypoint that wires the three subcommands
// (wrap, serve, attach) together. The actual command implementations live
// in internal/cli so the entrypoint stays a thin shell that's easy to
// audit and easy to test by swapping commands.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/SuperMarioYL/agentq/internal/cli"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "agentq:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "agentq",
		Short: "Fan approval prompts from N coding-agent sessions into one phone.",
		Long: `agentq runs three things:

  agentq wrap -- <agent>   intercept one agent's permission prompts
  agentq serve             run the local daemon that aggregates wrappers
  agentq attach            print a QR pointing at the local web triage queue

Run "agentq <command> --help" for details on a single subcommand.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	root.AddCommand(
		cli.NewWrapCmd(),
		cli.NewServeCmd(),
		cli.NewAttachCmd(),
	)

	return root
}
