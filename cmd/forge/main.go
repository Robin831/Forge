package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Robin831/Forge/internal/forge"
	"github.com/spf13/cobra"
)

// Global flags
var (
	configFile string
	jsonOutput bool
	verbose    bool
)

// Signal-aware context for graceful shutdown
var (
	rootCtx    context.Context
	rootCancel context.CancelFunc
)

func init() {
	// Persistent flags available to all subcommands
	rootCmd.PersistentFlags().StringVar(&configFile, "config", "", "Config file (default: forge.yaml in repo root)")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Root-only flags
	rootCmd.Flags().BoolP("version", "V", false, "Print version information")

	// Command groups for organized help output
	rootCmd.AddGroup(&cobra.Group{ID: "anvil", Title: "Repository Management:"})
	rootCmd.AddGroup(&cobra.Group{ID: "work", Title: "Work & Scheduling:"})
	rootCmd.AddGroup(&cobra.Group{ID: "daemon", Title: "Daemon & Monitoring:"})
	rootCmd.AddGroup(&cobra.Group{ID: "config", Title: "Configuration:"})

	// Register subcommands
	rootCmd.AddCommand(versionCmd)
}

var rootCmd = &cobra.Command{
	Use:   "forge",
	Short: "forge — autonomous AI coding orchestrator",
	Long: `The Forge is an autonomous multi-agent orchestrator for AI-assisted coding.
It manages worktrees, dispatches AI workers (Smiths), reviews output (Warden),
and monitors pull requests (Bellows) across registered repositories (Anvils).`,
	Run: func(cmd *cobra.Command, args []string) {
		if v, _ := cmd.Flags().GetBool("version"); v {
			printVersion()
			return
		}
		_ = cmd.Help()
	},
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Set up signal-aware context: SIGINT + SIGTERM cancel the context,
		// allowing in-flight work to drain gracefully.
		rootCtx, rootCancel = setupSignalContext()
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if rootCancel != nil {
			rootCancel()
		}
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		printVersion()
	},
}

func printVersion() {
	fmt.Printf("forge %s (build %s)\n", forge.Version, forge.Build)
}

// setupSignalContext returns a context that is cancelled on SIGINT or SIGTERM.
func setupSignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-sigCh:
			if verbose {
				fmt.Fprintf(os.Stderr, "\nReceived %s, shutting down...\n", sig)
			}
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
	}()

	return ctx, cancel
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
