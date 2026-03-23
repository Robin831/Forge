package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

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
		interval := ""
		if cfg != nil {
			interval = cfg.Settings.WicketInterval.String()
		}

		var repos []string
		if cfg != nil {
			for _, anvil := range cfg.Anvils {
				repos = append(repos, anvil.WicketRepos...)
			}
			sort.Strings(repos)
		}

		counts := make(map[string]int)
		for _, st := range []string{"pending", "bead_created", "ask_clarify", "needs_human"} {
			issues, err := db.ListWicketIssues(state.ListWicketIssuesOpts{State: st})
			if err == nil {
				counts[st] = len(issues)
			}
		}

		s := ipc.WicketStatusPayload{
			Enabled:        enabled,
			Interval:       interval,
			MonitoredRepos: repos,
			IssueCounts:    counts,
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
	if len(s.MonitoredRepos) > 0 {
		fmt.Fprintf(tw, "Monitored Repos\t%d\n", len(s.MonitoredRepos))
	} else {
		fmt.Fprintf(tw, "Monitored Repos\tderived from anvil git remotes\n")
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
		fmt.Println("\nMonitored Repos:")
		for _, r := range s.MonitoredRepos {
			fmt.Printf("  %s\n", r)
		}
	}
}
