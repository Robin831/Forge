package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/spf13/cobra"
)

func init() {
	queueRunCmd.Flags().StringP("anvil", "a", "", "Anvil name (to disambiguate if multiple anvils have the same bead ID)")
	queueClarifyCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	queueClarifyCmd.Flags().StringP("reason", "r", "", "Why clarification is needed")
	_ = queueClarifyCmd.MarkFlagRequired("anvil")
	_ = queueClarifyCmd.MarkFlagRequired("reason")
	queueUnclarifyCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	_ = queueUnclarifyCmd.MarkFlagRequired("anvil")
	queueRetryCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	_ = queueRetryCmd.MarkFlagRequired("anvil")
	queueStopCmd.Flags().StringP("anvil", "a", "", "Anvil name (required)")
	_ = queueStopCmd.MarkFlagRequired("anvil")
	queueStopCmd.Flags().StringP("reason", "r", "", "Why the bead is being stopped (optional)")
	queueCmd.AddCommand(queueRunCmd)
	queueCmd.AddCommand(queueClarifyCmd)
	queueCmd.AddCommand(queueUnclarifyCmd)
	queueCmd.AddCommand(queueRetryCmd)
	queueCmd.AddCommand(queueStopCmd)
	rootCmd.AddCommand(queueCmd)
}

var queueCmd = &cobra.Command{
	Use:     "queue",
	Short:   "Show ready beads across all anvils",
	GroupID: "work",
	RunE: func(cmd *cobra.Command, args []string) error {
		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}

		if len(cfg.Anvils) == 0 {
			fmt.Println("No anvils registered. Use 'forge anvil add <name> <path>' first.")
			return nil
		}

		p := poller.New(cfg.Anvils)
		beads, results := p.Poll(rootCtx)

		// Report per-anvil errors
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", r.Name, r.Err)
			}
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(beads)
		}

		if len(beads) == 0 {
			fmt.Println("No ready beads found across registered anvils.")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "PRIORITY\tANVIL\tID\tTITLE\n")
		for _, b := range beads {
			fmt.Fprintf(tw, "P%d\t%s\t%s\t%s\n", b.Priority, b.Anvil, b.ID, b.Title)
		}
		tw.Flush()

		fmt.Printf("\n%d ready beads across %d anvils\n", len(beads), len(cfg.Anvils))
		return nil
	},
}

var queueRunCmd = &cobra.Command{
	Use:     "run <id>",
	Short:   "Manually dispatch a bead for execution",
	Args:    cobra.ExactArgs(1),
	Example: "  forge queue run BD-42\n  forge queue run BD-42 --anvil metadata",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		anvil, _ := cmd.Flags().GetString("anvil")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.RunBeadPayload{
			BeadID: beadID,
			Anvil:  anvil,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "run_bead",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			if err := json.Unmarshal(resp.Payload, &msg); err != nil {
				return fmt.Errorf("daemon error (failed to unmarshal message): %w", err)
			}
			return fmt.Errorf("daemon error: %s", msg["message"])
		}

		var result map[string]string
		if err := json.Unmarshal(resp.Payload, &result); err != nil {
			return fmt.Errorf("failed to unmarshal daemon response: %w", err)
		}
		fmt.Printf("Successfully dispatched bead %s: %s\n", beadID, result["message"])
		return nil
	},
}

var queueClarifyCmd = &cobra.Command{
	Use:     "clarify <id>",
	Short:   "Mark a bead as needing human clarification before work can start (daemon-local only; bead still appears in anvil polling)",
	Args:    cobra.ExactArgs(1),
	Example: "  forge queue clarify BD-42 --anvil heimdall --reason 'which auth library?'",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		anvil, _ := cmd.Flags().GetString("anvil")
		reason, _ := cmd.Flags().GetString("reason")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.ClarificationPayload{
			BeadID: beadID,
			Anvil:  anvil,
			Reason: reason,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "set_clarification",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			var errMsg string
			if err := json.Unmarshal(resp.Payload, &msg); err == nil && msg["message"] != "" {
				errMsg = msg["message"]
			} else if len(resp.Payload) > 0 {
				errMsg = string(resp.Payload)
			} else {
				errMsg = "unknown error from daemon"
			}
			return fmt.Errorf("daemon error: %s", errMsg)
		}

		fmt.Printf("Bead %s marked as needing clarification\n", beadID)
		return nil
	},
}

var queueRetryCmd = &cobra.Command{
	Use:     "retry <id>",
	Short:   "Reset dispatch circuit breaker for a bead (clears needs_human from dispatch failures)",
	Args:    cobra.ExactArgs(1),
	Example: "  forge queue retry BD-42 --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		anvil, _ := cmd.Flags().GetString("anvil")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.RetryBeadPayload{
			BeadID: beadID,
			Anvil:  anvil,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			var errMsg string
			if err := json.Unmarshal(resp.Payload, &msg); err == nil && msg["message"] != "" {
				errMsg = msg["message"]
			} else if len(resp.Payload) > 0 {
				errMsg = string(resp.Payload)
			} else {
				errMsg = "unknown error from daemon"
			}
			return fmt.Errorf("daemon error: %s", errMsg)
		}

		fmt.Printf("Circuit breaker reset for bead %s — it will be retried on next poll\n", beadID)
		return nil
	},
}

var queueUnclarifyCmd = &cobra.Command{
	Use:     "unclarify <id>",
	Short:   "Clear the clarification_needed flag so the bead can be dispatched (daemon-local only)",
	Args:    cobra.ExactArgs(1),
	Example: "  forge queue unclarify BD-42 --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		anvil, _ := cmd.Flags().GetString("anvil")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.ClarificationPayload{
			BeadID: beadID,
			Anvil:  anvil,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "clear_clarification",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			var errMsg string
			if err := json.Unmarshal(resp.Payload, &msg); err == nil && msg["message"] != "" {
				errMsg = msg["message"]
			} else if len(resp.Payload) > 0 {
				errMsg = string(resp.Payload)
			} else {
				errMsg = "unknown error from daemon"
			}
			return fmt.Errorf("daemon error: %s", errMsg)
		}

		fmt.Printf("Clarification cleared for bead %s\n", beadID)
		return nil
	},
}

var queueStopCmd = &cobra.Command{
	Use:   "stop <id>",
	Short: "Fully stop a bead: kill worker, prevent re-dispatch, release to open",
	Long: `Stop all processing of a bead. This will:
  1. Kill any running worker for the bead
  2. Mark the bead as needing clarification (prevents auto and manual dispatch)
  3. Release the bead back to open status

The bead will not be dispatched again until you run 'forge queue unclarify'.`,
	Args:    cobra.ExactArgs(1),
	Example: "  forge queue stop BD-42 --anvil heimdall\n  forge queue stop BD-42 --anvil heimdall --reason 'wrong approach'",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		anvil, _ := cmd.Flags().GetString("anvil")
		reason, _ := cmd.Flags().GetString("reason")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.StopBeadPayload{
			BeadID: beadID,
			Anvil:  anvil,
			Reason: reason,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "stop_bead",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			var errMsg string
			if err := json.Unmarshal(resp.Payload, &msg); err == nil && msg["message"] != "" {
				errMsg = msg["message"]
			} else if len(resp.Payload) > 0 {
				errMsg = string(resp.Payload)
			} else {
				errMsg = "unknown error from daemon"
			}
			return fmt.Errorf("daemon error: %s", errMsg)
		}

		fmt.Printf("Bead %s stopped — use 'forge queue unclarify --anvil %s %s' to resume\n", beadID, anvil, beadID)
		return nil
	},
}
