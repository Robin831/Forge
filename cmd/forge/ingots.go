package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/spf13/cobra"
)

// truncateTitle returns title truncated to at most maxRunes runes, appending
// "..." when truncation occurs. Truncation is rune-safe (multi-byte characters
// and emoji are not split).
func truncateTitle(title string, maxRunes int) string {
	if utf8.RuneCountInString(title) > maxRunes {
		runes := []rune(title)
		return string(runes[:maxRunes-3]) + "..."
	}
	return title
}

// temperDisplay returns the display string for the temper column in the
// ingots list table.
func temperDisplay(status string, passed bool) string {
	switch {
	case status == string(ingot.StatusInit) || status == string(ingot.StatusSmith):
		return "--"
	case passed:
		return "pass"
	default:
		return "FAIL"
	}
}

func init() {
	ingotsListCmd.Flags().StringP("anvil", "a", "", "Filter by anvil name")
	ingotsListCmd.Flags().StringP("status", "s", "", "Filter by status (init, smith, temper, warden, approved, pr_open, pr_merged, failed, stalled)")

	ingotsShowCmd.Flags().StringP("anvil", "a", "", "Anvil name (to disambiguate if bead exists in multiple anvils)")

	ingotsCmd.AddCommand(ingotsListCmd)
	ingotsCmd.AddCommand(ingotsShowCmd)
	rootCmd.AddCommand(ingotsCmd)
}

var ingotsCmd = &cobra.Command{
	Use:     "ingots",
	Short:   "Query ingot records (bead lifecycle snapshots)",
	GroupID: "work",
}

var ingotsListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List ingots with optional filters",
	Example: "  forge ingots list\n  forge ingots list --anvil heimdall\n  forge ingots list --status pr_open\n  forge ingots list --anvil heimdall --status failed",
	RunE: func(cmd *cobra.Command, args []string) error {
		anvil, _ := cmd.Flags().GetString("anvil")
		status, _ := cmd.Flags().GetString("status")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.GetIngotsPayload{
			Anvil:  anvil,
			Status: status,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "get_ingots",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			if err := json.Unmarshal(resp.Payload, &msg); err != nil {
				return fmt.Errorf("daemon error")
			}
			return fmt.Errorf("daemon error: %s", msg["message"])
		}

		var ingots []ingot.Ingot
		if err := json.Unmarshal(resp.Payload, &ingots); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(ingots)
		}

		if len(ingots) == 0 {
			fmt.Println("No ingots found.")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "BEAD ID\tANVIL\tSTATUS\tTITLE\tPR#\tTEMPER\n")
		for _, ig := range ingots {
			pr := "--"
			if ig.PRNumber != nil {
				pr = fmt.Sprintf("#%d", *ig.PRNumber)
			}
			temper := temperDisplay(string(ig.Status), ig.TemperPassed)
			title := truncateTitle(ig.Title, 50)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				ig.BeadID, ig.Anvil, string(ig.Status), title, pr, temper)
		}
		tw.Flush()

		fmt.Printf("\n%d ingot(s)\n", len(ingots))
		return nil
	},
}

var ingotsShowCmd = &cobra.Command{
	Use:     "show <bead-id>",
	Short:   "Show detailed ingot information with test results",
	Args:    cobra.ExactArgs(1),
	Example: "  forge ingots show Forge-abc1\n  forge ingots show Forge-abc1 --anvil heimdall",
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		anvil, _ := cmd.Flags().GetString("anvil")

		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w (is 'forge up' running?)", err)
		}
		defer client.Close()

		payload, _ := json.Marshal(ipc.GetIngotPayload{
			BeadID: beadID,
			Anvil:  anvil,
		})

		resp, err := client.Send(ipc.Command{
			Type:    "get_ingot",
			Payload: payload,
		})
		if err != nil {
			return fmt.Errorf("sending command: %w", err)
		}

		if resp.Type == "error" {
			var msg map[string]string
			if err := json.Unmarshal(resp.Payload, &msg); err != nil {
				return fmt.Errorf("daemon error")
			}
			return fmt.Errorf("daemon error: %s", msg["message"])
		}

		var ig ingot.Ingot
		if err := json.Unmarshal(resp.Payload, &ig); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(ig)
		}

		// Header
		statusStr := string(ig.Status)
		if ig.PRNumber != nil {
			statusStr = fmt.Sprintf("%s (#%d)", ig.Status, *ig.PRNumber)
		}
		fmt.Printf("Ingot: %s (%s)\n", ig.BeadID, ig.Anvil)
		fmt.Printf("Status:   %s\n", statusStr)
		if ig.Title != "" {
			fmt.Printf("Title:    %s\n", ig.Title)
		}
		if ig.Branch != "" {
			fmt.Printf("Branch:   %s\n", ig.Branch)
		}

		// Temper results
		if len(ig.TestResults) > 0 {
			fmt.Printf("\nTemper Results:\n")
			for _, tr := range ig.TestResults {
				verdict := "PASS"
				if !tr.Passed {
					verdict = "FAIL"
				}
				name := tr.StepName
				if tr.Optional {
					name += " (optional)"
				}
				duration := fmt.Sprintf("%.1fs", float64(tr.DurationMs)/1000.0)
				fmt.Printf("  %s %-20s %7s   exit=%d\n", verdict, name, duration, tr.ExitCode)
			}
		}

		// PR link
		if ig.PRURL != "" {
			fmt.Println()
			if ig.PRNumber != nil {
				fmt.Printf("PR #%d: %s\n", *ig.PRNumber, ig.PRURL)
			} else {
				fmt.Printf("PR: %s\n", ig.PRURL)
			}
		}

		return nil
	},
}
