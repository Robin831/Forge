package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	wicketListCmd.Flags().StringP("repo", "r", "", "Filter by repository (owner/repo)")
	wicketListCmd.Flags().StringP("status", "s", "", "Filter by lifecycle state (e.g. pending, bead_created, ask_clarify, needs_human, rejected)")
	wicketListCmd.Flags().IntP("limit", "n", 50, "Maximum number of issues to show (0 = no limit)")
	wicketCmd.AddCommand(wicketListCmd)
}

var wicketListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Wicket-tracked GitHub issues",
	Long: `Lists GitHub issues tracked by the Wicket triage monitor, with their
lifecycle state and triage decisions.

States: pending, bead_created, ask_clarify, needs_human, rejected, dispatched, stale, merged, closed`,
	Example: `  forge wicket list
  forge wicket list --repo owner/repo
  forge wicket list --status pending
  forge wicket list --status bead_created --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		status, _ := cmd.Flags().GetString("status")
		limit, _ := cmd.Flags().GetInt("limit")

		// Try live IPC if daemon is running.
		_, running := daemon.IsRunning()
		if running {
			client, err := ipc.NewClient()
			if err == nil {
				defer client.Close()
				payload, _ := json.Marshal(ipc.WicketListPayload{
					Repo:   repo,
					Status: status,
					Limit:  limit,
				})
				resp, err := client.Send(ipc.Command{Type: "wicket_list", Payload: payload})
				if err == nil && resp.Type == "ok" {
					var r ipc.WicketListResponse
					if err := json.Unmarshal(resp.Payload, &r); err == nil {
						if jsonOutput {
							fmt.Println(string(resp.Payload))
							return nil
						}
						printWicketList(r.Issues)
						return nil
					}
				}
			}
		}

		// Fallback: read from state DB directly.
		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		opts := state.ListWicketIssuesOpts{
			Repo:  repo,
			State: status,
			Limit: limit,
		}
		issues, err := db.ListWicketIssues(opts)
		if err != nil {
			return fmt.Errorf("listing wicket issues: %w", err)
		}

		items := make([]ipc.WicketIssueItem, 0, len(issues))
		for _, wi := range issues {
			items = append(items, ipc.WicketIssueItem{
				ID:           wi.ID,
				Repo:         wi.Repo,
				IssueNumber:  wi.IssueNumber,
				Title:        wi.Title,
				Author:       wi.Author,
				State:        wi.State,
				TriageAction: wi.TriageAction,
				TriageReason: wi.TriageReason,
				BeadID:       wi.BeadID,
				PRNumber:     wi.PRNumber,
				PRUrl:        wi.PRUrl,
				CreatedAt:    wi.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				UpdatedAt:    wi.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			})
		}

		if jsonOutput {
			r := ipc.WicketListResponse{Issues: items}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(r)
		}

		printWicketList(items)
		return nil
	},
}

func printWicketList(issues []ipc.WicketIssueItem) {
	if len(issues) == 0 {
		fmt.Println("No wicket issues found.")
		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "REPO\tISSUE\tSTATE\tACTION\tTITLE\n")
	for _, wi := range issues {
		title := wi.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(tw, "%s\t#%d\t%s\t%s\t%s\n",
			wi.Repo,
			wi.IssueNumber,
			wi.State,
			wi.TriageAction,
			title,
		)
	}
	tw.Flush()
	fmt.Printf("\n%d issue(s)\n", len(issues))
}
