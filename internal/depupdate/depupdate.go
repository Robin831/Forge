// Package depupdate provides direct dependency update functionality that
// bypasses the Smith AI worker. It reuses depcheck scanners to discover
// outdated packages, groups them, and (in future sub-tasks) applies updates
// directly via package manager commands.
package depupdate

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/state"
)

// Options controls which updates are included in a scan.
type Options struct {
	// PatchOnly limits results to patch-level updates only.
	PatchOnly bool
	// NoMajor excludes major version updates from results.
	NoMajor bool
}

// AnvilResult holds scan results for a single anvil, potentially spanning
// multiple ecosystems (Go, npm, NuGet).
type AnvilResult struct {
	Anvil      string
	Path       string
	Ecosystems []*depcheck.CheckResult
}

// TotalUpdates returns the total number of outdated dependencies across all
// ecosystems for this anvil, respecting the given filter options.
func (ar *AnvilResult) TotalUpdates(opts Options) int {
	total := 0
	for _, cr := range ar.Ecosystems {
		total += len(filterUpdates(cr, opts))
	}
	return total
}

// Runner orchestrates dependency scanning across anvils using the existing
// depcheck scanners. Future sub-tasks will add update execution, grouping,
// and PR creation methods.
type Runner struct {
	scanner *depcheck.Scanner
}

// NewRunner creates a Runner that uses the provided depcheck Scanner for
// discovering outdated dependencies.
func NewRunner(db *state.DB, anvilPaths map[string]string, cfg *config.SettingsConfig) *Runner {
	timeout := cfg.DepcheckTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	// Use a one-hour interval — the CLI is one-shot and the ticker never fires again.
	scanner := depcheck.New(db, time.Hour, timeout, anvilPaths)
	return &Runner{
		scanner: scanner,
	}
}

// Scan runs dependency checks on all configured anvils and returns the
// aggregated results. Every anvil is included in the output so that callers
// can distinguish "no supported ecosystems" from "no updates" from "scan error".
func (r *Runner) Scan(ctx context.Context, anvilPaths map[string]string) []AnvilResult {
	var results []AnvilResult
	for name, path := range anvilPaths {
		if ctx.Err() != nil {
			break
		}
		ecosystems := r.scanner.ScanAnvilDeps(ctx, name, path)
		results = append(results, AnvilResult{
			Anvil:      name,
			Path:       path,
			Ecosystems: ecosystems,
		})
	}
	return results
}

// PrintSummary writes a human-readable summary of outdated dependencies to w.
// Returns the total number of updates displayed.
func PrintSummary(w io.Writer, results []AnvilResult, opts Options) int {
	totalUpdates := 0

	for _, ar := range results {
		anvilHasUpdates := false

		for _, cr := range ar.Ecosystems {
			if cr.Error != nil {
				if !anvilHasUpdates {
					fmt.Fprintf(w, "\nAnvil: %s\n", ar.Anvil)
					anvilHasUpdates = true
				}
				fmt.Fprintf(w, "  %s (scan error: %v)\n", cr.Ecosystem, cr.Error)
				continue
			}

			updates := filterUpdates(cr, opts)
			if len(updates) == 0 {
				continue
			}

			if !anvilHasUpdates {
				fmt.Fprintf(w, "\nAnvil: %s\n", ar.Anvil)
				anvilHasUpdates = true
			}

			fmt.Fprintf(w, "  %s (%d outdated)\n", cr.Ecosystem, len(updates))
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "    PACKAGE\tCURRENT\tLATEST\tTYPE\n")
			for _, u := range updates {
				fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", u.Path, u.Current, u.Latest, u.Kind)
			}
			tw.Flush()
			totalUpdates += len(updates)
		}

		if !anvilHasUpdates {
			msg := "all dependencies up to date"
			if opts.PatchOnly {
				msg = "no dependency updates matching patch-only filter"
			} else if opts.NoMajor {
				msg = "no dependency updates matching non-major filter"
			}
			fmt.Fprintf(w, "\nAnvil: %s — %s\n", ar.Anvil, msg)
		}
	}

	return totalUpdates
}

// FormatSummaryLine returns a one-line summary like "12 outdated dependencies across 3 anvils".
func FormatSummaryLine(results []AnvilResult, opts Options) string {
	totalUpdates := 0
	anvils := 0
	for _, ar := range results {
		count := ar.TotalUpdates(opts)
		if count > 0 {
			anvils++
		}
		totalUpdates += count
	}

	if totalUpdates == 0 {
		if opts.PatchOnly {
			return "0 outdated (patch only) across all anvils."
		} else if opts.NoMajor {
			return "0 outdated (excluding major) across all anvils."
		}
		return "All dependencies up to date across all anvils."
	}

	parts := []string{fmt.Sprintf("%d outdated", totalUpdates)}
	if opts.PatchOnly {
		parts = append(parts, "(patch only)")
	} else if opts.NoMajor {
		parts = append(parts, "(excluding major)")
	}
	parts = append(parts, fmt.Sprintf("across %d anvil(s)", anvils))
	return strings.Join(parts, " ")
}

// filterUpdates returns the subset of updates from a CheckResult that match
// the given filter options, in Major→Minor→Patch order to match depcheck's
// established severity ordering.
func filterUpdates(cr *depcheck.CheckResult, opts Options) []depcheck.ModuleUpdate {
	var updates []depcheck.ModuleUpdate
	if !opts.PatchOnly && !opts.NoMajor {
		updates = append(updates, cr.Major...)
	}
	if !opts.PatchOnly {
		updates = append(updates, cr.Minor...)
	}
	updates = append(updates, cr.Patch...)
	return updates
}
