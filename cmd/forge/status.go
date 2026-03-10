package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

var briefOutput bool

func init() {
	statusCmd.Flags().BoolVar(&briefOutput, "brief", false, "One-line output suitable for shell prompts or status bars")
	rootCmd.AddCommand(statusCmd)
}

// formatBrief renders a single-line status summary.
// Example: "⚒ 2 smiths | 5 queued | 3 PRs | $1.23 | polled 30s ago"
// When daemon is down: "⚒ DOWN"
func formatBrief(running bool, workers, queueSize, openPRs int, dailyCost float64, lastPoll string) string {
	if !running {
		return "⚒ DOWN"
	}
	line := fmt.Sprintf("⚒ %d smiths | %d queued | %d PRs", workers, queueSize, openPRs)
	if dailyCost > 0 {
		line += fmt.Sprintf(" | $%.2f", dailyCost)
	}
	if lastPoll != "" && lastPoll != "n/a" {
		line += fmt.Sprintf(" | polled %s", lastPoll)
	}
	return line
}

var statusCmd = &cobra.Command{
	Use:     "status",
	Short:   "Show current daemon and worker status",
	Long:    "Shows daemon health, active workers, queue size, and cost information.\n\nUse --brief for a single-line summary suitable for shell prompts or status bars:\n  forge status --brief\n  # Output: ⚒ 2 smiths | 5 queued | 3 PRs | $1.23 | polled 30s ago",
	GroupID: "daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		pid, running := daemon.IsRunning()

		// Try IPC for live status if daemon is running
		if running {
			client, err := ipc.NewClient()
			if err == nil {
				defer client.Close()
				resp, err := client.Send(ipc.Command{Type: "status"})
				if err == nil && resp.Type == "status" {
					if jsonOutput {
						fmt.Println(string(resp.Payload))
						return nil
					}
					var s ipc.StatusPayload
					_ = json.Unmarshal(resp.Payload, &s)

					if briefOutput {
						fmt.Println(formatBrief(true, s.Workers, s.QueueSize, s.OpenPRs, s.DailyCost, s.LastPoll))
						return nil
					}

					tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
					fmt.Fprintf(tw, "Daemon\tRunning (PID %d)\n", s.PID)
					fmt.Fprintf(tw, "Uptime\t%s\n", s.Uptime)
					fmt.Fprintf(tw, "Workers\t%d active\n", s.Workers)
					fmt.Fprintf(tw, "Queue\t%d beads\n", s.QueueSize)
					fmt.Fprintf(tw, "Open PRs\t%d\n", s.OpenPRs)
					fmt.Fprintf(tw, "Last Poll\t%s\n", s.LastPoll)
					if s.DailyCostLimit > 0 {
						fmt.Fprintf(tw, "Daily Cost\t$%.2f / $%.2f\n", s.DailyCost, s.DailyCostLimit)
						if s.CostLimitPaused {
							fmt.Fprintf(tw, "Cost Status\tauto-dispatch paused (limit reached)\n")
						}
					} else if s.DailyCost > 0 {
						fmt.Fprintf(tw, "Daily Cost\t$%.2f (no limit)\n", s.DailyCost)
					}
					tw.Flush()

					if len(s.Quotas) > 0 {
						fmt.Printf("\nProvider Quotas:\n")
						tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
						fmt.Fprintf(tw, "PROVIDER\tREQUESTS\tTOKENS\tRESET\n")
						for pv, q := range s.Quotas {
							reqStr := fmt.Sprintf("%d/%d", q.RequestsRemaining, q.RequestsLimit)
							tokStr := fmt.Sprintf("%d/%d", q.TokensRemaining, q.TokensLimit)
							resetStr := "n/a"
							if q.RequestsReset != nil {
								resetStr = time.Until(*q.RequestsReset).Round(time.Minute).String()
							}
							fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", pv, reqStr, tokStr, resetStr)
						}
						tw.Flush()
					}
					return nil
				}
			}
		}

		// Brief mode without daemon: just report down
		if briefOutput && !running {
			fmt.Println(formatBrief(false, 0, 0, 0, 0, ""))
			return nil
		}

		// Fallback: read from state DB directly
		db, err := state.Open("")
		if err != nil {
			if briefOutput {
				fmt.Println(formatBrief(running, 0, 0, 0, 0, ""))
				return nil
			}
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		workers, _ := db.ActiveWorkers()
		prs, _ := db.OpenPRs()
		events, _ := db.RecentEvents(5)
		quotas, _ := db.GetAllProviderQuotas()
		todayCost, _ := db.GetTodayCost()

		if briefOutput {
			fmt.Println(formatBrief(running, len(workers), 0, len(prs), todayCost, ""))
			return nil
		}

		type statusData struct {
			DaemonRunning bool                      `json:"daemon_running"`
			DaemonPID     int                       `json:"daemon_pid,omitempty"`
			ActiveWorkers int                       `json:"active_workers"`
			OpenPRs       int                       `json:"open_prs"`
			RecentEvents  int                       `json:"recent_events"`
			DBPath        string                    `json:"db_path"`
			Quotas        map[string]provider.Quota `json:"quotas,omitempty"`
			DailyCost     float64                   `json:"daily_cost"`
		}

		data := statusData{
			DaemonRunning: running,
			DaemonPID:     pid,
			ActiveWorkers: len(workers),
			OpenPRs:       len(prs),
			RecentEvents:  len(events),
			DBPath:        db.Path(),
			Quotas:        quotas,
			DailyCost:     todayCost,
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(data)
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		if running {
			fmt.Fprintf(tw, "Daemon\tRunning (PID %d)\n", pid)
		} else {
			fmt.Fprintf(tw, "Daemon\tStopped\n")
		}
		fmt.Fprintf(tw, "Workers\t%d active\n", len(workers))
		fmt.Fprintf(tw, "Open PRs\t%d\n", len(prs))
		if todayCost > 0 {
			fmt.Fprintf(tw, "Daily Cost\t$%.2f\n", todayCost)
		}
		fmt.Fprintf(tw, "DB\t%s\n", db.Path())
		tw.Flush()

		if len(quotas) > 0 {
			fmt.Printf("\nProvider Quotas:\n")
			tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "PROVIDER\tREQUESTS\tTOKENS\tRESET\n")
			for pv, q := range quotas {
				reqStr := fmt.Sprintf("%d/%d", q.RequestsRemaining, q.RequestsLimit)
				tokStr := fmt.Sprintf("%d/%d", q.TokensRemaining, q.TokensLimit)
				resetStr := "n/a"
				if q.RequestsReset != nil {
					resetStr = time.Until(*q.RequestsReset).Round(time.Minute).String()
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", pv, reqStr, tokStr, resetStr)
			}
			tw.Flush()
		}

		if len(workers) > 0 {
			fmt.Printf("\nActive Workers:\n")
			tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "ID\tBEAD\tANVIL\tSTATUS\tRUNNING\n")
			for _, w := range workers {
				dur := time.Since(w.StartedAt).Round(time.Second)
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					w.ID, w.BeadID, w.Anvil, w.Status, dur)
			}
			tw.Flush()
		}

		if len(events) > 0 {
			fmt.Printf("\nRecent Events:\n")
			for _, e := range events {
				age := time.Since(e.Timestamp).Round(time.Second)
				fmt.Printf("  [%s ago] %s: %s\n", age, e.Type, e.Message)
			}
		}

		return nil
	},
}
