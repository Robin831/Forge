package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	backfillAnvilsCmd.Flags().Bool("dry-run", false, "Show what would change without writing to the database")
	rootCmd.AddCommand(backfillAnvilsCmd)
}

var backfillAnvilsCmd = &cobra.Command{
	Use:     "backfill-anvils",
	Short:   "Heuristically populate empty anvil on legacy forge_sessions",
	GroupID: "config",
	Long: `Scans forge_sessions rows where anvil = '' and tries to populate the
anvil column by case-insensitive substring matching of the session title
against the registered anvil names from forge.yaml.

Behavior per row:
  - exactly one anvil name matches the title → UPDATE the row
  - multiple anvil names match              → leave untouched, log as ambiguous
  - no anvil names match                    → leave untouched, log as no-match

Prints a summary of scanned / updated / skipped rows at the end. Use
--dry-run to preview without writing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if cfg == nil {
			loaded, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			cfg = loaded
		}
		anvilNames := sortedAnvilNames(cfg.Anvils)
		if len(anvilNames) == 0 {
			return fmt.Errorf("no anvils registered; nothing to match against")
		}

		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		sessions, err := db.ListForgeSessionsMissingAnvil()
		if err != nil {
			return fmt.Errorf("listing sessions with empty anvil: %w", err)
		}

		type result struct {
			ID      int64    `json:"id"`
			Title   string   `json:"title"`
			Anvil   string   `json:"anvil,omitempty"`
			Matches []string `json:"matches,omitempty"`
			Outcome string   `json:"outcome"` // updated | ambiguous | no_match | error
			Err     string   `json:"error,omitempty"`
		}

		results := make([]result, 0, len(sessions))
		var updated, ambiguous, noMatch, errs int
		for _, s := range sessions {
			matches := matchAnvil(s.Title, anvilNames)
			r := result{ID: s.ID, Title: s.Title, Matches: matches}
			switch {
			case len(matches) == 1:
				r.Anvil = matches[0]
				if dryRun {
					r.Outcome = "updated"
					updated++
				} else if err := db.UpdateForgeSessionAnvil(s.ID, matches[0]); err != nil {
					r.Outcome = "error"
					r.Err = err.Error()
					errs++
				} else {
					r.Outcome = "updated"
					updated++
				}
			case len(matches) > 1:
				r.Outcome = "ambiguous"
				ambiguous++
			default:
				r.Outcome = "no_match"
				noMatch++
			}
			results = append(results, r)
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{
				"dry_run":            dryRun,
				"scanned":            len(sessions),
				"updated":            updated,
				"skipped_ambiguous":  ambiguous,
				"skipped_no_match":   noMatch,
				"errors":             errs,
				"rows":               results,
			})
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "ID\tOUTCOME\tANVIL\tTITLE\n")
		for _, r := range results {
			detail := r.Anvil
			switch r.Outcome {
			case "ambiguous":
				detail = "[" + strings.Join(r.Matches, ", ") + "]"
			case "no_match":
				detail = "-"
			case "error":
				detail = "ERR: " + r.Err
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", r.ID, r.Outcome, detail, truncate(r.Title, 60))
		}
		tw.Flush()

		mode := "applied"
		if dryRun {
			mode = "dry-run"
		}
		fmt.Printf("\nSummary (%s): scanned=%d updated=%d skipped_ambiguous=%d skipped_no_match=%d errors=%d\n",
			mode, len(sessions), updated, ambiguous, noMatch, errs)

		if errs > 0 {
			return fmt.Errorf("%d row(s) failed to update", errs)
		}
		return nil
	},
}

// matchAnvil returns the registered anvil names whose lowercase name occurs as
// a substring of the lowercase title. The result preserves the input order of
// anvilNames so callers get stable, sorted output.
func matchAnvil(title string, anvilNames []string) []string {
	lower := strings.ToLower(title)
	out := []string{}
	for _, name := range anvilNames {
		if name == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(name)) {
			out = append(out, name)
		}
	}
	return out
}

// sortedAnvilNames extracts the keys of the anvil map in lexicographic order so
// matchAnvil's output is stable across runs.
func sortedAnvilNames(anvils map[string]config.AnvilConfig) []string {
	names := make([]string, 0, len(anvils))
	for name := range anvils {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

