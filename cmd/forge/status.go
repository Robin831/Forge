package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:     "status",
	Short:   "Show current daemon and worker status",
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
					tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
					fmt.Fprintf(tw, "Daemon\tRunning (PID %d)\n", s.PID)
					fmt.Fprintf(tw, "Uptime\t%s\n", s.Uptime)
					fmt.Fprintf(tw, "Workers\t%d active\n", s.Workers)
					fmt.Fprintf(tw, "Queue\t%d beads\n", s.QueueSize)
					fmt.Fprintf(tw, "Open PRs\t%d\n", s.OpenPRs)
					tw.Flush()
					return nil
				}
			}
		}

		// Fallback: read from state DB directly
		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		workers, _ := db.ActiveWorkers()
		prs, _ := db.OpenPRs()
		events, _ := db.RecentEvents(5)

		type statusData struct {
			DaemonRunning bool   `json:"daemon_running"`
			DaemonPID     int    `json:"daemon_pid,omitempty"`
			ActiveWorkers int    `json:"active_workers"`
			OpenPRs       int    `json:"open_prs"`
			RecentEvents  int    `json:"recent_events"`
			DBPath        string `json:"db_path"`
		}

		data := statusData{
			DaemonRunning: running,
			DaemonPID:     pid,
			ActiveWorkers: len(workers),
			OpenPRs:       len(prs),
			RecentEvents:  len(events),
			DBPath:        db.Path(),
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
		fmt.Fprintf(tw, "DB\t%s\n", db.Path())
		tw.Flush()

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
