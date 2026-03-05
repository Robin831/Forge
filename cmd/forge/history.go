package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

var historyLimit int

func init() {
	historyCmd.Flags().IntVarP(&historyLimit, "limit", "n", 20, "Number of entries to show")
	historyCmd.AddCommand(historyWorkersCmd)
	historyCmd.AddCommand(historyEventsCmd)
	rootCmd.AddCommand(historyCmd)
}

var historyCmd = &cobra.Command{
	Use:     "history",
	Short:   "Show completed work and event log",
	GroupID: "daemon",
	RunE:    runHistoryWorkers, // Default: show workers
}

var historyWorkersCmd = &cobra.Command{
	Use:   "workers",
	Short: "Show completed workers",
	RunE:  runHistoryWorkers,
}

var historyEventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Show event log",
	RunE:  runHistoryEvents,
}

func runHistoryWorkers(cmd *cobra.Command, args []string) error {
	db, err := state.Open("")
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer db.Close()

	workers, err := db.CompletedWorkers(historyLimit)
	if err != nil {
		return fmt.Errorf("querying completed workers: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(workers)
	}

	if len(workers) == 0 {
		fmt.Println("No completed workers found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "ID\tBEAD\tANVIL\tSTATUS\tDURATION\tCOMPLETED\n")
	for _, w := range workers {
		duration := "?"
		completed := "?"
		if w.CompletedAt != nil {
			duration = w.CompletedAt.Sub(w.StartedAt).Round(time.Second).String()
			completed = w.CompletedAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			w.ID, w.BeadID, w.Anvil, w.Status, duration, completed)
	}
	tw.Flush()

	fmt.Printf("\n%d completed workers (limit %d)\n", len(workers), historyLimit)
	return nil
}

func runHistoryEvents(cmd *cobra.Command, args []string) error {
	db, err := state.Open("")
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer db.Close()

	events, err := db.RecentEvents(historyLimit)
	if err != nil {
		return fmt.Errorf("querying events: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(events)
	}

	if len(events) == 0 {
		fmt.Println("No events recorded.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "TIME\tTYPE\tMESSAGE\tBEAD\tANVIL\n")
	for _, e := range events {
		ts := e.Timestamp.Format("2006-01-02 15:04:05")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ts, e.Type, truncate(e.Message, 50), e.BeadID, e.Anvil)
	}
	tw.Flush()

	fmt.Printf("\n%d events (limit %d)\n", len(events), historyLimit)
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
