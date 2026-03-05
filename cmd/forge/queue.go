package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/spf13/cobra"
)

func init() {
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
