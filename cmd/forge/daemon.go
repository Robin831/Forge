package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/spf13/cobra"
)

func init() {
	upCmd.Flags().Bool("foreground", false, "Run daemon in foreground (for debugging)")

	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
}

var upCmd = &cobra.Command{
	Use:     "up",
	Short:   "Start the Forge daemon",
	Long:    "Starts The Forge daemon as a background process. It polls anvils for beads, spawns workers, and monitors PRs.",
	GroupID: "daemon",
	Run: func(cmd *cobra.Command, args []string) {
		// Check if already running
		pid, running := daemon.IsRunning()
		if running {
			if jsonOutput {
				out, _ := json.Marshal(map[string]any{
					"status": "already_running",
					"pid":    pid,
				})
				fmt.Println(string(out))
			} else {
				fmt.Fprintf(os.Stderr, "Forge daemon already running (PID %d)\n", pid)
			}
			os.Exit(1)
		}

		// Start daemon in background by re-executing ourselves with a hidden flag
		foreground, _ := cmd.Flags().GetBool("foreground")
		if foreground {
			// Run in foreground (used by the background spawn and for debugging)
			d, err := daemon.New(cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error initializing daemon: %v\n", err)
				os.Exit(1)
			}
			if err := d.Run(rootCtx); err != nil {
				fmt.Fprintf(os.Stderr, "Daemon error: %v\n", err)
				os.Exit(1)
			}
			return
		}

		// Spawn background process
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error finding executable: %v\n", err)
			os.Exit(1)
		}

		spawnArgs := []string{"up", "--foreground"}
		if configFile != "" {
			spawnArgs = append(spawnArgs, "--config", configFile)
		}

		bgCmd := exec.Command(exe, spawnArgs...)
		bgCmd.Stdout = nil
		bgCmd.Stderr = nil
		bgCmd.Stdin = nil

		// Detach on Windows
		if runtime.GOOS == "windows" {
			detachProcess(bgCmd)
		}

		if err := bgCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting daemon: %v\n", err)
			os.Exit(1)
		}

		// Capture PID before releasing
		bgPid := bgCmd.Process.Pid

		// Detach the child process so it survives parent exit
		_ = bgCmd.Process.Release()

		if jsonOutput {
			out, _ := json.Marshal(map[string]any{
				"status": "started",
				"pid":    bgPid,
			})
			fmt.Println(string(out))
		} else {
			fmt.Printf("Forge daemon started (PID %d)\n", bgPid)
		}
	},
}

var downCmd = &cobra.Command{
	Use:     "down",
	Short:   "Stop the Forge daemon",
	Long:    "Sends a graceful shutdown signal to the running Forge daemon.",
	GroupID: "daemon",
	Run: func(cmd *cobra.Command, args []string) {
		pid, running := daemon.IsRunning()
		if !running {
			if jsonOutput {
				out, _ := json.Marshal(map[string]any{
					"status": "not_running",
				})
				fmt.Println(string(out))
			} else {
				fmt.Fprintf(os.Stderr, "No Forge daemon running\n")
			}
			os.Exit(1)
		}

		if err := daemon.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "Error stopping daemon: %v\n", err)
			os.Exit(1)
		}

		if jsonOutput {
			out, _ := json.Marshal(map[string]any{
				"status": "stopping",
				"pid":    pid,
			})
			fmt.Println(string(out))
		} else {
			fmt.Printf("Forge daemon stopping (PID %d)\n", pid)
		}
	},
}
