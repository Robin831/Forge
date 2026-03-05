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
	queueCmd.AddCommand(queueRunCmd)
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
