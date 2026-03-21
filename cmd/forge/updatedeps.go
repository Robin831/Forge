package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depupdate"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	updateDepsCmd.Flags().StringP("anvil", "a", "", "Update deps only in the named anvil (default: all)")
	updateDepsCmd.Flags().Bool("patch-only", false, "Only include patch-level updates")
	updateDepsCmd.Flags().Bool("no-major", false, "Exclude major version updates")
	updateDepsCmd.Flags().Bool("dry-run", false, "Show what would be updated without making changes")
	rootCmd.AddCommand(updateDepsCmd)
}

var updateDepsCmd = &cobra.Command{
	Use:   "update-deps",
	Short: "Scan and update outdated dependencies across anvils",
	Long: `Scans all registered anvils for outdated dependencies using the existing
depcheck scanners (Go, npm, NuGet). Displays a summary of available updates,
then prompts for how to proceed.

Use --dry-run to preview grouped updates without prompting. Use --patch-only or
--no-major to pre-filter which updates are included before the prompt.

Interactive prompt choices:
  [a]ll           — apply all filtered updates
  [p]atch+minor   — apply only patch and minor updates (excludes majors)
  [s]elect groups — choose specific update groups by number
  [n]o            — exit without changes`,
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

		// Build anvil path map, filtering to a specific anvil if requested.
		anvilPaths := make(map[string]string)
		if anvilName, _ := cmd.Flags().GetString("anvil"); anvilName != "" {
			a, ok := cfg.Anvils[anvilName]
			if !ok {
				return fmt.Errorf("unknown anvil %q", anvilName)
			}
			anvilPaths[anvilName] = a.Path
		} else {
			for name, a := range cfg.Anvils {
				anvilPaths[name] = a.Path
			}
		}

		opts := depupdate.Options{}
		opts.PatchOnly, _ = cmd.Flags().GetBool("patch-only")
		opts.NoMajor, _ = cmd.Flags().GetBool("no-major")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state db: %w", err)
		}
		defer db.Close()

		runner := depupdate.NewRunner(db, anvilPaths, &cfg.Settings)

		fmt.Fprintf(os.Stderr, "Scanning %d anvil(s) for outdated dependencies...\n", len(anvilPaths))
		results := runner.Scan(rootCtx, anvilPaths)

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}

		if len(results) == 0 {
			fmt.Println("\nAll dependencies up to date across all anvils. Your deps are as solid as a freshly forged blade!")
			return nil
		}

		totalUpdates := depupdate.PrintSummary(os.Stdout, results, opts)

		fmt.Printf("\n%s\n", depupdate.FormatSummaryLine(results, opts))

		if totalUpdates == 0 {
			return nil
		}

		// Build UpdateGroups from scan results, pre-filtered by --patch-only / --no-major.
		groups := depupdate.BuildFilteredGroups(rootCtx, results, opts)

		if len(groups) == 0 {
			fmt.Println("No update groups to apply after filtering.")
			return nil
		}

		// --dry-run: display groups and exit without prompting.
		if dryRun {
			fmt.Printf("\n%d update group(s) would be applied:\n", len(groups))
			for i, g := range groups {
				fmt.Printf("  %2d. %-40s  %s  (%d package(s))\n", i+1, g.Name, g.Kind, len(g.Updates))
			}
			return nil
		}

		// Interactive prompt: let the user choose which groups to apply.
		selected, err := depupdate.PromptSelection(os.Stdin, os.Stdout, groups)
		if err != nil {
			return fmt.Errorf("selection prompt: %w", err)
		}
		if len(selected) == 0 {
			return nil
		}

		// Hand off selected groups to the execution pipeline.
		// Sibling sub-tasks will wire in install/verify/commit/PR steps here.
		return runUpdateGroups(rootCtx, selected)
	},
}

// runUpdateGroups is the entry point for the update execution pipeline.
// Sibling sub-tasks (install, verify, commit, PR) will be wired in here;
// for now it prints a summary of what was selected.
func runUpdateGroups(ctx context.Context, groups []depupdate.UpdateGroup) error {
	_ = ctx // will be used by future sub-tasks
	fmt.Printf("\n%d group(s) selected for update:\n", len(groups))
	for _, g := range groups {
		fmt.Printf("  • %s (%s, %d package(s))\n", g.Name, g.Kind, len(g.Updates))
	}
	fmt.Println("\nUpdate execution will be wired in by sibling sub-tasks.")
	return nil
}
