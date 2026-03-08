// Package depcheck periodically checks registered anvils for outdated
// dependencies, starting with Go and designed to support additional ecosystems
// (.NET, npm) in the future. When updates are found it creates beads so a
// Smith agent can apply them. Patch/minor updates produce auto-dispatch beads;
// major version bumps produce "needs attention" beads.
package depcheck

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// ModuleUpdate describes a single outdated dependency.
type ModuleUpdate struct {
	Path    string // module/package path
	Current string // current version
	Latest  string // latest available version
	Kind    string // "patch", "minor", or "major"
}

// CheckResult holds the depcheck results for a single anvil.
type CheckResult struct {
	Anvil     string
	Path      string
	Ecosystem string         // e.g. "Go", ".NET", "npm"
	Patch     []ModuleUpdate // patch updates (auto-bead)
	Minor     []ModuleUpdate // minor updates (auto-bead)
	Major     []ModuleUpdate // major version bumps (needs attention)
	Error     error
	Checked   time.Time
}

// Scanner checks anvils for outdated dependencies across all supported ecosystems.
type Scanner struct {
	db         *state.DB
	interval   time.Duration
	timeout    time.Duration
	anvilPaths map[string]string // anvil name -> path
}

// New creates a dependency check scanner.
func New(db *state.DB, interval, timeout time.Duration, anvilPaths map[string]string) *Scanner {
	if interval < 1*time.Hour {
		interval = 1 * time.Hour
	}
	if timeout < 1*time.Minute {
		timeout = 1 * time.Minute
	}
	return &Scanner{
		db:         db,
		interval:   interval,
		timeout:    timeout,
		anvilPaths: anvilPaths,
	}
}

// Run starts the periodic check loop. Blocks until ctx is canceled.
func (s *Scanner) Run(ctx context.Context) error {
	log.Printf("[depcheck] Starting dependency checker (interval: %s, timeout: %s)", s.interval, s.timeout)
	_ = s.db.LogEvent(state.EventDepcheckStarted,
		fmt.Sprintf("Dependency checker started (interval: %s)", s.interval), "", "")

	// Initial check
	s.ScanAll(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[depcheck] Shutting down dependency checker")
			return ctx.Err()
		case <-ticker.C:
			s.ScanAll(ctx)
		}
	}
}

// ScanAll runs dependency checks on all anvils across all supported ecosystems.
func (s *Scanner) ScanAll(ctx context.Context) {
	log.Printf("[depcheck] Checking %d anvils for outdated dependencies", len(s.anvilPaths))

	for name, path := range s.anvilPaths {
		if ctx.Err() != nil {
			return
		}
		s.scanAnvil(ctx, name, path)
	}
}

// scanAnvil runs all applicable ecosystem scanners for a single anvil and
// creates beads for any outdated dependencies found.
func (s *Scanner) scanAnvil(ctx context.Context, name, path string) {
	// Run each ecosystem scanner. Each returns nil if the ecosystem is not
	// present (e.g. no go.mod → scanGo returns nil).
	scanners := []struct {
		name string
		fn   func(ctx context.Context, anvil, path string) *CheckResult
	}{
		{"Go", s.scanGo},
		// Future ecosystem scanners are added here:
		// {"dotnet", s.scanDotnet},
		// {"npm", s.scanNpm},
	}

	for _, sc := range scanners {
		if ctx.Err() != nil {
			return
		}

		result := sc.fn(ctx, name, path)
		if result == nil {
			continue // ecosystem not present in this anvil
		}

		if result.Error != nil {
			log.Printf("[depcheck] Error checking %s (%s): %v", name, sc.name, result.Error)
			_ = s.db.LogEvent(state.EventDepcheckFailed,
				fmt.Sprintf("Dependency check failed for %s (%s): %v", name, sc.name, result.Error), "", name)
			continue
		}

		total := len(result.Patch) + len(result.Minor) + len(result.Major)
		if total == 0 {
			log.Printf("[depcheck] %s (%s): all dependencies up to date", name, sc.name)
			_ = s.db.LogEvent(state.EventDepcheckPassed,
				fmt.Sprintf("All %s dependencies up to date in %s", sc.name, name), "", name)
			continue
		}

		log.Printf("[depcheck] %s (%s): %d outdated (%d patch, %d minor, %d major)",
			name, sc.name, total, len(result.Patch), len(result.Minor), len(result.Major))
		_ = s.db.LogEvent(state.EventDepcheckFound,
			fmt.Sprintf("Found %d outdated %s dependencies in %s (%d patch, %d minor, %d major)",
				total, sc.name, name, len(result.Patch), len(result.Minor), len(result.Major)),
			"", name)

		s.createBeads(ctx, result)
	}
}

// createBeads creates bead issues for the outdated dependencies found in an anvil.
// Each module gets its own bead so that dedup, PR tracking, and agent assignment
// are per-module. A DedupCache is built once per anvil to avoid spawning one
// external command per module.
func (s *Scanner) createBeads(ctx context.Context, result *CheckResult) {
	cache := BuildDedupCache(ctx, s.db, result.Path, result.Anvil)

	for _, u := range append(result.Patch, result.Minor...) {
		if DedupCheckWithCache(cache, u.Path) {
			log.Printf("[depcheck] %s: bead/PR already exists for %s, skipping", result.Anvil, u.Path)
			continue
		}
		s.createUpdateBead(ctx, result, "auto", u)
	}

	for _, u := range result.Major {
		if DedupCheckWithCache(cache, u.Path) {
			log.Printf("[depcheck] %s: bead/PR already exists for %s, skipping", result.Anvil, u.Path)
			continue
		}
		s.createUpdateBead(ctx, result, "major", u)
	}
}

// createUpdateBead runs 'bd create' to create a bead for a single dependency update.
func (s *Scanner) createUpdateBead(ctx context.Context, result *CheckResult, kind string, update ModuleUpdate) {
	title := BeadTitle(result.Ecosystem, update.Path, update.Current, update.Latest)
	var priority string
	var desc strings.Builder

	switch kind {
	case "auto":
		priority = "3"
		desc.WriteString(fmt.Sprintf("Automated %s dependency update: %s %s → %s.\n\n",
			result.Ecosystem, update.Path, update.Current, update.Latest))
		desc.WriteString("## Module\n\n")
		desc.WriteString("| Module | Current | Latest | Type |\n")
		desc.WriteString("|--------|---------|--------|------|\n")
		desc.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", update.Path, update.Current, update.Latest, update.Kind))
		desc.WriteString("\n## Instructions\n\n")
		desc.WriteString("```bash\n")
		desc.WriteString(fmt.Sprintf("go get %s@%s\n", update.Path, update.Latest))
		desc.WriteString("go mod tidy\n")
		desc.WriteString("```\n")
	case "major":
		priority = "2"
		desc.WriteString(fmt.Sprintf("%s major version update: %s %s → %s. This may contain breaking changes and requires manual review.\n\n",
			result.Ecosystem, update.Path, update.Current, update.Latest))
		desc.WriteString("## Module\n\n")
		desc.WriteString("| Module | Current | Latest | Type |\n")
		desc.WriteString("|--------|---------|--------|------|\n")
		desc.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", update.Path, update.Current, update.Latest, update.Kind))
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	issueType := "chore"
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "create",
		fmt.Sprintf("--title=%s", title),
		fmt.Sprintf("--description=%s", desc.String()),
		fmt.Sprintf("--type=%s", issueType),
		fmt.Sprintf("--priority=%s", priority),
		"--json",
	))
	cmd.Dir = result.Path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] Failed to create bead for %s (%s): %v: %s", result.Anvil, kind, err, stderr.String())
		_ = s.db.LogEvent(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to create %s update bead for %s: %v", kind, result.Anvil, err), "", result.Anvil)
		return
	}

	log.Printf("[depcheck] Created %s update bead for %s (%s): %s", kind, result.Anvil, update.Path, strings.TrimSpace(string(output)))
	_ = s.db.LogEvent(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Created %s dependency update bead for %s (%s %s → %s)", kind, result.Anvil, update.Path, update.Current, update.Latest),
		"", result.Anvil)
}
