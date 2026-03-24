package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depupdate"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/spf13/cobra"
)

func init() {
	updateDepsCmd.Flags().StringP("anvil", "a", "", "Update deps only in the named anvil (default: all)")
	updateDepsCmd.Flags().Bool("patch-only", false, "Only include patch-level updates")
	updateDepsCmd.Flags().Bool("no-major", false, "Exclude major version updates")
	updateDepsCmd.Flags().Bool("dry-run", false, "Show what would be updated without making changes")
	updateDepsCmd.Flags().Bool("create-pr", false, "Apply updates and create a GitHub PR for each anvil")
	updateDepsCmd.Flags().BoolP("yes", "y", false, "Auto-accept all update groups without prompting")
	rootCmd.AddCommand(updateDepsCmd)
}

var updateDepsCmd = &cobra.Command{
	Use:   "update-deps",
	Short: "Scan and update outdated dependencies across anvils",
	Long: `Scans all registered anvils for outdated dependencies using the existing
depcheck scanners (Go, npm, NuGet). Displays a summary of available updates.

Use --dry-run to preview updates without making changes. Use --patch-only or
--no-major to limit which updates are included.

Use --create-pr to apply updates end-to-end: for each anvil, a batch-update
branch is created from the remote default branch, then each update group is
presented interactively (y/N). Accepted groups are installed, verified with
Temper (build + lint + test), and committed individually; groups that fail
verification are rolled back. Accepted and passing groups are then pushed and
a GitHub PR is opened. Use --yes (-y) to auto-accept all groups without
per-group prompts.`,
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
		createPR, _ := cmd.Flags().GetBool("create-pr")
		if dryRun || totalUpdates == 0 || !createPR {
			return nil
		}

		yesAll, _ := cmd.Flags().GetBool("yes")

		// Compute the batch-update branch name and a per-run worktree ID once so
		// all anvils land on the same branch name (consistent same-day re-run
		// semantics) and the main anvil directory stays on main throughout.
		dateStr := time.Now().Format("2006-01-02")
		targetBranch := "deps/batch-update-" + dateStr
		worktreeBeadID := "depupdate-" + dateStr
		wtMgr := worktree.NewManager()

		// For each anvil with updates: create an isolated worktree, prompt user,
		// apply updates inside the worktree, generate changelog, create PR, and
		// close matching dep beads. The worktree is removed after each anvil.
		for _, ar := range results {
			if ar.TotalUpdates(opts) == 0 {
				continue
			}
			// Group all updates, then apply the same filters used for display so
			// the PR body and changelog only reflect what was shown to the user.
			groups := depupdate.FilterGroups(depupdate.GroupUpdates(rootCtx, ar.Ecosystems), opts)
			if len(groups) == 0 {
				continue
			}

			// processAnvil runs all dep-update steps for a single anvil inside an
			// isolated git worktree, keeping the main anvil directory on main.
			// A closure is used so defer cleanup is scoped to this anvil's work.
			func() {
				// Step 1: Create an isolated worktree on the batch-update branch so
				// that commits never land on main in the main repo directory.
				wt, err := wtMgr.CreateWithOptions(rootCtx, ar.Path, worktreeBeadID, worktree.CreateOptions{
					Branch:      targetBranch,
					ResetBranch: true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: worktree for %s: %v\n", ar.Anvil, err)
					return
				}
				defer wtMgr.Remove(context.Background(), ar.Path, wt)

				// Step 2: Interactive group selection — skip groups the user declines.
				selected := depupdate.SelectGroups(os.Stdin, os.Stdout, groups, yesAll)
				if len(selected) == 0 {
					fmt.Printf("No updates selected for %s.\n", ar.Anvil)
					return
				}

				// Step 3: Execute — install, verify (Temper), commit or rollback.
				anvilCfg := cfg.Anvils[ar.Anvil]
				applied := depupdate.ExecuteGroups(rootCtx, wt.Path, anvilCfg, selected)
				if len(applied) == 0 {
					fmt.Fprintf(os.Stderr, "No updates successfully applied for %s.\n", ar.Anvil)
					return
				}
				fmt.Printf("Applied %d group(s) for %s.\n", len(applied), ar.Anvil)

				// Step 4: Generate changelog fragment and commit it on the branch.
				isBilingual := depupdate.DetectBilingual(wt.Path)
				if err := depupdate.GenerateChangelog(wt.Path, applied, isBilingual); err != nil {
					fmt.Fprintf(os.Stderr, "warning: changelog for %s: %v\n", ar.Anvil, err)
					return
				}

				// Step 5: Push branch and open the PR.
				prURL, err := depupdate.CreatePR(rootCtx, wt.Path, ar.Anvil, targetBranch, applied)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: PR for %s: %v\n", ar.Anvil, err)
					return
				}
				fmt.Printf("PR for %s: %s\n", ar.Anvil, prURL)

				// Step 6: Close any open dep-check beads covered by this batch.
				if err := depupdate.CloseMatchingDepBeads(rootCtx, wt.Path, applied); err != nil {
					fmt.Fprintf(os.Stderr, "warning: closing dep beads for %s: %v\n", ar.Anvil, err)
				}
			}()
		}

		return nil
	},
}
