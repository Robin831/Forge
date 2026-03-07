package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	depsCheckCmd.Flags().Bool("create-beads", true, "Auto-create beads for discovered updates")
	depsCheckCmd.Flags().StringP("anvil", "a", "", "Check only the named anvil (default: all)")

	depsCmd.AddCommand(depsCheckCmd)
	rootCmd.AddCommand(depsCmd)
}

var depsCmd = &cobra.Command{
	Use:     "deps",
	Short:   "Dependency management commands",
	GroupID: "work",
}

var depsCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Scan registered anvils for outdated dependencies",
	Long: `Scans all registered anvils (or a specific one) for outdated dependencies
across Go, .NET/NuGet, and npm ecosystems. Outdated packages are reported and
optionally turned into beads — patch/minor updates grouped per ecosystem,
major version bumps as separate beads.`,
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

		// Build anvil paths map and disabled set
		anvilPaths := make(map[string]string)
		disabled := make(map[string]bool)
		for name, anvil := range cfg.Anvils {
			anvilPaths[name] = anvil.Path
			if anvil.DepcheckEnabled != nil && !*anvil.DepcheckEnabled {
				disabled[name] = true
			}
		}

		// Filter to a specific anvil if requested
		if anvilName, _ := cmd.Flags().GetString("anvil"); anvilName != "" {
			a, ok := cfg.Anvils[anvilName]
			if !ok {
				return fmt.Errorf("unknown anvil %q", anvilName)
			}
			anvilPaths = map[string]string{anvilName: a.Path}
			// Respect per-anvil disable even when filtering
			newDisabled := make(map[string]bool)
			if disabled[anvilName] {
				newDisabled[anvilName] = true
			}
			disabled = newDisabled
		}

		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state db: %w", err)
		}
		defer db.Close()

		monitor := depcheck.New(db,
			cfg.Settings.DepcheckInterval,
			cfg.Settings.DepcheckTimeout,
			anvilPaths,
			disabled)

		results := monitor.CheckAll(rootCtx)

		// Create beads if requested
		createBeads, _ := cmd.Flags().GetBool("create-beads")
		if createBeads {
			created, err := monitor.CreateBeads(rootCtx, results)
			if err != nil {
				return fmt.Errorf("creating beads: %w", err)
			}
			if !jsonOutput && created > 0 {
				fmt.Printf("\nCreated %d new beads for dependency updates\n", created)
			}
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(results)
		}

		// Display results
		totalUpdates := 0
		anyErr := false
		for _, r := range results {
			if r.Error != nil {
				fmt.Fprintf(os.Stderr, "Error checking %s: %v\n", r.Anvil, r.Error)
				anyErr = true
				continue
			}

			if len(r.Updates) == 0 {
				fmt.Printf("%s: all dependencies up to date\n", r.Anvil)
				continue
			}

			fmt.Printf("\n%s: %d outdated dependencies\n", r.Anvil, len(r.Updates))
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "  ECOSYSTEM\tPACKAGE\tCURRENT\tLATEST\tTYPE\n")
			for _, u := range r.Updates {
				pkg := u.Package
				if u.Subdir != "" {
					pkg = fmt.Sprintf("%s (%s)", u.Package, u.Subdir)
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
					u.Ecosystem, pkg, u.Current, u.Latest, u.Kind)
			}
			tw.Flush()
			totalUpdates += len(r.Updates)
		}

		if totalUpdates == 0 && !anyErr {
			fmt.Println("\nAll anvils clean — no outdated dependencies found. Your deps are as solid as forged steel!")
		}

		return nil
	},
}
