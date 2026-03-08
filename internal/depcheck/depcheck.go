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
	Anvil   string
	Path    string
	Patch   []ModuleUpdate // patch updates (auto-bead)
	Minor   []ModuleUpdate // minor updates (auto-bead)
	Major   []ModuleUpdate // major version bumps (needs attention)
	Error   error
	Checked time.Time
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

// dedupCheck returns true if an open bead whose title contains phrase already
// exists in the anvil's bead queue. This prevents duplicate update beads from
// accumulating across check cycles. It is shared by all ecosystem scanners.
func (s *Scanner) dedupCheck(ctx context.Context, anvilPath, phrase string) bool {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "list", "--status=open", "--json"))
	cmd.Dir = anvilPath

	output, err := cmd.Output()
	if err != nil {
		// If we can't query, allow creation rather than silently suppressing beads.
		return false
	}

	return strings.Contains(string(output), phrase)
}

// createBeads creates bead issues for the outdated dependencies found in an anvil.
// Patch+minor updates go into a single auto-dispatch bead.
// Major updates go into a separate "needs attention" bead.
// Duplicate beads (same kind already open) are skipped via dedupCheck.
func (s *Scanner) createBeads(ctx context.Context, result *CheckResult) {
	if len(result.Patch)+len(result.Minor) > 0 {
		phrase := fmt.Sprintf("%s dependencies (patch/minor)", ecosystemLabel(result))
		if !s.dedupCheck(ctx, result.Path, phrase) {
			s.createUpdateBead(ctx, result, "auto",
				append(result.Patch, result.Minor...))
		} else {
			log.Printf("[depcheck] %s: open patch/minor update bead already exists, skipping", result.Anvil)
		}
	}

	if len(result.Major) > 0 {
		phrase := fmt.Sprintf("%s major version updates", ecosystemLabel(result))
		if !s.dedupCheck(ctx, result.Path, phrase) {
			s.createUpdateBead(ctx, result, "major", result.Major)
		} else {
			log.Printf("[depcheck] %s: open major update bead already exists, skipping", result.Anvil)
		}
	}
}

// createUpdateBead runs 'bd create' to create a bead for dependency updates.
func (s *Scanner) createUpdateBead(ctx context.Context, result *CheckResult, kind string, updates []ModuleUpdate) {
	label := ecosystemLabel(result)
	var title string
	var priority string
	var desc strings.Builder

	switch kind {
	case "auto":
		title = fmt.Sprintf("Update %d %s dependencies (patch/minor)", len(updates), label)
		priority = "3"
		desc.WriteString(fmt.Sprintf("Automated dependency update for %s patch and minor version bumps.\n\n", label))
		desc.WriteString("Run the appropriate update commands for each module below, then tidy.\n\n")
	case "major":
		title = fmt.Sprintf("Review %d %s major version updates", len(updates), label)
		priority = "2"
		desc.WriteString(fmt.Sprintf("%s major version updates detected. These may contain breaking changes and require manual review.\n\n", label))
	}

	desc.WriteString("## Outdated Modules\n\n")
	desc.WriteString("| Module | Current | Latest | Type |\n")
	desc.WriteString("|--------|---------|--------|------|\n")
	for _, u := range updates {
		desc.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", u.Path, u.Current, u.Latest, u.Kind))
	}

	if kind == "auto" {
		desc.WriteString("\n## Instructions\n\n")
		desc.WriteString("```bash\n")
		for _, u := range updates {
			desc.WriteString(fmt.Sprintf("go get %s@%s\n", u.Path, u.Latest))
		}
		desc.WriteString("go mod tidy\n")
		desc.WriteString("```\n")
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

	log.Printf("[depcheck] Created %s update bead for %s: %s", kind, result.Anvil, strings.TrimSpace(string(output)))
	_ = s.db.LogEvent(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Created %s dependency update bead for %s (%d modules)", kind, result.Anvil, len(updates)),
		"", result.Anvil)
}

// ecosystemLabel returns a human-readable label for the ecosystem that produced
// the given CheckResult. Currently always "Go" but will extend to ".NET" and
// "npm" as those scanners are added.
func ecosystemLabel(_ *CheckResult) string {
	// For now all results come from scanGo. When scanDotnet / scanNpm are
	// added, CheckResult will carry an Ecosystem field and this function
	// will switch on it.
	return "Go"
}
