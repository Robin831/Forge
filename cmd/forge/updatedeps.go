package main

import (
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
depcheck scanners (Go, npm, NuGet). Displays a summary of available updates.

Use --dry-run to preview updates without making changes. Use --patch-only or
--no-major to limit which updates are included.

Future versions will support interactive selection, grouped updates, temper
verification, and automatic PR creation.`,
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

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		if dryRun || totalUpdates == 0 {
			return nil
		}

		// For each anvil with updates, generate a changelog fragment and open a PR.
		for _, ar := range results {
			if ar.TotalUpdates(opts) == 0 {
				continue
			}
			groups := depupdate.GroupUpdates(rootCtx, ar.Ecosystems)
			if len(groups) == 0 {
				continue
			}
			isBilingual := depupdate.DetectBilingual(ar.Path)
			if err := depupdate.GenerateChangelog(ar.Path, groups, isBilingual); err != nil {
				fmt.Fprintf(os.Stderr, "warning: changelog for %s: %v\n", ar.Anvil, err)
				continue
			}
			prURL, err := depupdate.CreatePR(rootCtx, ar.Path, ar.Anvil, groups)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: PR for %s: %v\n", ar.Anvil, err)
				continue
			}
			fmt.Printf("PR for %s: %s\n", ar.Anvil, prURL)
		}

		return nil
	},
}
