package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	wicketCmd.AddCommand(wicketStatusCmd)
	rootCmd.AddCommand(wicketCmd)
}

var wicketCmd = &cobra.Command{
	Use:     "wicket",
	Short:   "Manage the Wicket GitHub issue triage monitor",
	GroupID: "work",
}

var wicketStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Wicket issue triage monitor status",
	Long: `Shows the Wicket issue triage monitor status: enabled state,
monitored repositories, issue counts by triage state, and configuration.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Try live IPC if daemon is running.
		_, running := daemon.IsRunning()
		if running {
			client, err := ipc.NewClient()
			if err == nil {
				defer client.Close()
				resp, err := client.Send(ipc.Command{Type: "wicket_status"})
				if err == nil && resp.Type == "ok" {
					var s ipc.WicketStatusPayload
					if err := json.Unmarshal(resp.Payload, &s); err == nil {
						if jsonOutput {
							fmt.Println(string(resp.Payload))
							return nil
						}
						printWicketStatus(s)
						return nil
					}
				}
			}
		}

		// Fallback: read from state DB and config directly.
		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		enabled := cfg != nil && cfg.Settings.WicketEnabled

		// Compute the effective interval, applying the same defaulting logic as
		// the Wicket monitor (interval <= 0 falls back to a 15m default).
		effectiveInterval := time.Duration(0)
		if cfg != nil {
			effectiveInterval = cfg.Settings.WicketInterval
		}
		if effectiveInterval <= 0 {
			effectiveInterval = 15 * time.Minute
		}
		interval := effectiveInterval.String()

		// Collect explicitly configured repos; count anvils that derive from git remote.
		var repos []string
		derivedAnvils := 0
		if cfg != nil {
			repoSet := make(map[string]struct{})
			for _, anvil := range cfg.Anvils {
				if len(anvil.WicketRepos) > 0 {
					for _, r := range anvil.WicketRepos {
						if r != "" {
							repoSet[r] = struct{}{}
						}
					}
				} else {
					derivedAnvils++
				}
			}
			for r := range repoSet {
				repos = append(repos, r)
			}
			sort.Strings(repos)
		}

		counts := make(map[string]int)
		for _, st := range []string{"pending", "bead_created", "ask_clarify", "needs_human"} {
			n, err := db.CountWicketIssues(state.ListWicketIssuesOpts{State: st})
			if err == nil {
				counts[st] = n
			}
		}

		lastScan, _ := db.LastWicketScanAt()

		s := ipc.WicketStatusPayload{
			Enabled:        enabled,
			Interval:       interval,
			MonitoredRepos: repos,
			DerivedAnvils:  derivedAnvils,
			IssueCounts:    counts,
			LastScanAt:     lastScan,
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(s)
		}

		printWicketStatus(s)
		return nil
	},
}

func printWicketStatus(s ipc.WicketStatusPayload) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	enabledStr := "disabled"
	if s.Enabled {
		enabledStr = "enabled"
	}
	fmt.Fprintf(tw, "Wicket\t%s\n", enabledStr)
	if s.Interval != "" {
		fmt.Fprintf(tw, "Poll Interval\t%s\n", s.Interval)
	}
	if s.LastScanAt != nil {
		fmt.Fprintf(tw, "Last Scan\t%s\n", s.LastScanAt.Local().Format("2006-01-02 15:04:05"))
	}
	if len(s.MonitoredRepos) > 0 {
		fmt.Fprintf(tw, "Configured Repos\t%d\n", len(s.MonitoredRepos))
	} else {
		fmt.Fprintf(tw, "Configured Repos\t(none listed; may be derived from anvil git remotes)\n")
	}
	if s.DerivedAnvils > 0 {
		fmt.Fprintf(tw, "Derived Repos\t%d anvil(s) derive repo from git remote at runtime\n", s.DerivedAnvils)
	}
	tw.Flush()

	if len(s.IssueCounts) > 0 {
		fmt.Println("\nIssue Counts by State:")
		tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "STATE\tCOUNT\n")
		for _, st := range []string{"pending", "bead_created", "ask_clarify", "needs_human"} {
			if n, ok := s.IssueCounts[st]; ok {
				fmt.Fprintf(tw, "%s\t%d\n", st, n)
			}
		}
		tw.Flush()
	}

	if len(s.MonitoredRepos) > 0 {
		fmt.Println("\nConfigured Repos:")
		for _, r := range s.MonitoredRepos {
			fmt.Printf("  %s\n", r)
		}
	}
}
