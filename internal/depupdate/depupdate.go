// Package depupdate provides direct dependency update functionality that
// bypasses the Smith AI worker. It reuses depcheck scanners to discover
// outdated packages, groups them, applies updates via package-manager commands,
// verifies them with Temper, and wires changelog generation + PR creation.
package depupdate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
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

// FilterGroups returns only the groups whose Kind is allowed by opts.
// Groups whose kind is excluded by PatchOnly or NoMajor are dropped.
func FilterGroups(groups []UpdateGroup, opts Options) []UpdateGroup {
	if !opts.PatchOnly && !opts.NoMajor {
		return groups
	}
	filtered := groups[:0:0]
	for _, g := range groups {
		if opts.PatchOnly && g.Kind != "patch" {
			continue
		}
		if opts.NoMajor && g.Kind == "major" {
			continue
		}
		filtered = append(filtered, g)
	}
	return filtered
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

// SelectGroups presents each UpdateGroup to the user and collects a yes/no
// response. When yesAll is true every group is accepted without prompting. The
// function reads responses from r and writes prompts to w.
func SelectGroups(r io.Reader, w io.Writer, groups []UpdateGroup, yesAll bool) []UpdateGroup {
	if yesAll {
		return groups
	}
	scanner := bufio.NewScanner(r)
	var selected []UpdateGroup
	for _, g := range groups {
		pkgWord := "package"
		if len(g.Updates) != 1 {
			pkgWord = "packages"
		}
		fmt.Fprintf(w, "Apply %q (%s, %d %s)? [y/N] ", g.Name, g.Kind, len(g.Updates), pkgWord)
		if !scanner.Scan() {
			break
		}
		if strings.EqualFold(strings.TrimSpace(scanner.Text()), "y") {
			selected = append(selected, g)
		}
	}
	return selected
}

// ExecuteGroups applies each UpdateGroup to the given anvil by installing
// packages, running Temper verification, and committing on success or rolling
// back on failure. It returns the subset of groups that were successfully
// applied and committed.
func ExecuteGroups(ctx context.Context, anvilPath string, anvilCfg config.AnvilConfig, groups []UpdateGroup) []UpdateGroup {
	var applied []UpdateGroup
	for _, g := range groups {
		if ctx.Err() != nil {
			log.Printf("[depupdate] context cancelled — stopping before group %q", g.Name)
			break
		}
		if err := installGroup(ctx, anvilPath, g); err != nil {
			log.Printf("[depupdate] install failed for group %q: %v", g.Name, err)
			if rbErr := RollbackGroup(ctx, anvilPath, g, err); rbErr != nil {
				log.Printf("[depupdate] rollback failed for group %q: %v", g.Name, rbErr)
			}
			continue
		}
		result, verifyErr := VerifyGroup(ctx, anvilPath, anvilCfg)
		if verifyErr != nil {
			log.Printf("[depupdate] verify error for group %q: %v — rolling back", g.Name, verifyErr)
			if rbErr := RollbackGroup(ctx, anvilPath, g, verifyErr); rbErr != nil {
				log.Printf("[depupdate] rollback failed for group %q: %v", g.Name, rbErr)
			}
			continue
		}
		if !result.Passed {
			log.Printf("[depupdate] verify failed for group %q — rolling back", g.Name)
			if rbErr := RollbackGroup(ctx, anvilPath, g, fmt.Errorf("temper verification failed")); rbErr != nil {
				log.Printf("[depupdate] rollback failed for group %q: %v", g.Name, rbErr)
			}
			continue
		}
		if err := CommitGroup(ctx, anvilPath, g); err != nil {
			log.Printf("[depupdate] commit failed for group %q: %v — rolling back", g.Name, err)
			if rbErr := RollbackGroup(ctx, anvilPath, g, err); rbErr != nil {
				log.Printf("[depupdate] rollback failed for group %q: %v", g.Name, rbErr)
			}
			continue
		}
		applied = append(applied, g)
	}
	return applied
}

// installGroup dispatches to the appropriate ecosystem-specific installer.
func installGroup(ctx context.Context, anvilPath string, g UpdateGroup) error {
	switch g.Ecosystem {
	case "npm":
		return InstallNpmGroup(ctx, anvilPath, g)
	case "Go":
		return InstallGoGroup(ctx, anvilPath, g)
	case "NuGet", ".NET":
		return InstallDotnetGroup(ctx, anvilPath, g)
	default:
		return fmt.Errorf("unknown ecosystem %q for group %q", g.Ecosystem, g.Name)
	}
}
