// Package depcheck periodically checks Go modules for outdated dependencies
// across registered anvils. When updates are found, it creates beads so a Smith
// agent can apply the updates. Patch/minor updates produce auto-dispatch beads;
// major version bumps produce "needs attention" beads.
package depcheck

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// ModuleUpdate describes an outdated Go module dependency.
type ModuleUpdate struct {
	Path    string // module path
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

// Monitor periodically checks Go anvils for outdated dependencies.
type Monitor struct {
	db         *state.DB
	interval   time.Duration
	timeout    time.Duration
	anvilPaths map[string]string // anvil name -> path
}

// New creates a dependency check monitor.
func New(db *state.DB, interval, timeout time.Duration, anvilPaths map[string]string) *Monitor {
	if interval < 1*time.Hour {
		interval = 1 * time.Hour
	}
	if timeout < 1*time.Minute {
		timeout = 5 * time.Minute
	}
	return &Monitor{
		db:         db,
		interval:   interval,
		timeout:    timeout,
		anvilPaths: anvilPaths,
	}
}

// Run starts the periodic check loop. Blocks until ctx is canceled.
func (m *Monitor) Run(ctx context.Context) error {
	log.Printf("[depcheck] Starting dependency checker (interval: %s, timeout: %s)", m.interval, m.timeout)
	_ = m.db.LogEvent(state.EventDepcheckStarted,
		fmt.Sprintf("Dependency checker started (interval: %s)", m.interval), "", "")

	// Initial check
	m.checkAll(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[depcheck] Shutting down dependency checker")
			return ctx.Err()
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// checkAll runs dependency checks on all Go anvils.
func (m *Monitor) checkAll(ctx context.Context) {
	log.Printf("[depcheck] Checking %d anvils for outdated dependencies", len(m.anvilPaths))

	for name, path := range m.anvilPaths {
		if ctx.Err() != nil {
			return
		}

		result := m.checkAnvil(ctx, name, path)
		if result.Error != nil {
			log.Printf("[depcheck] Error checking %s: %v", name, result.Error)
			_ = m.db.LogEvent(state.EventDepcheckFailed,
				fmt.Sprintf("Dependency check failed for %s: %v", name, result.Error), "", name)
			continue
		}

		total := len(result.Patch) + len(result.Minor) + len(result.Major)
		if total == 0 {
			log.Printf("[depcheck] %s: all dependencies up to date", name)
			_ = m.db.LogEvent(state.EventDepcheckPassed,
				fmt.Sprintf("All dependencies up to date in %s", name), "", name)
			continue
		}

		log.Printf("[depcheck] %s: %d outdated (%d patch, %d minor, %d major)",
			name, total, len(result.Patch), len(result.Minor), len(result.Major))
		_ = m.db.LogEvent(state.EventDepcheckFound,
			fmt.Sprintf("Found %d outdated dependencies in %s (%d patch, %d minor, %d major)",
				total, name, len(result.Patch), len(result.Minor), len(result.Major)),
			"", name)

		m.createBeads(ctx, result)
	}
}

// checkAnvil runs 'go list -m -u all' in an anvil directory if it has a go.mod.
func (m *Monitor) checkAnvil(ctx context.Context, name, path string) CheckResult {
	result := CheckResult{
		Anvil:   name,
		Path:    path,
		Checked: time.Now(),
	}

	// Only check Go projects
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
		return result // not a Go project, skip silently
	}

	cmdCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "go", "list", "-m", "-u", "all"))
	cmd.Dir = path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		result.Error = fmt.Errorf("go list -m -u all: %w: %s", err, stderr.String())
		return result
	}

	updates := parseGoListOutput(string(output))
	for _, u := range updates {
		switch u.Kind {
		case "patch":
			result.Patch = append(result.Patch, u)
		case "minor":
			result.Minor = append(result.Minor, u)
		case "major":
			result.Major = append(result.Major, u)
		}
	}

	return result
}

// parseGoListOutput parses the output of 'go list -m -u all' and returns
// modules that have updates available. Each output line looks like:
//
//	github.com/foo/bar v1.2.3 [v1.4.0]
//
// Lines without brackets have no update. The first line (the main module) is skipped.
func parseGoListOutput(output string) []ModuleUpdate {
	var updates []ModuleUpdate

	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // skip main module
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Look for [vX.Y.Z] update marker
		bracketStart := strings.Index(line, "[")
		bracketEnd := strings.Index(line, "]")
		if bracketStart < 0 || bracketEnd < 0 || bracketEnd <= bracketStart {
			continue // no update available
		}

		latest := line[bracketStart+1 : bracketEnd]
		prefix := strings.TrimSpace(line[:bracketStart])
		parts := strings.Fields(prefix)
		if len(parts) < 2 {
			continue
		}

		modPath := parts[0]
		current := parts[1]

		kind := classifyUpdate(current, latest)

		updates = append(updates, ModuleUpdate{
			Path:    modPath,
			Current: current,
			Latest:  latest,
			Kind:    kind,
		})
	}

	sort.Slice(updates, func(i, j int) bool {
		if updates[i].Kind != updates[j].Kind {
			// major first (needs attention), then minor, then patch
			order := map[string]int{"major": 0, "minor": 1, "patch": 2}
			return order[updates[i].Kind] < order[updates[j].Kind]
		}
		return updates[i].Path < updates[j].Path
	})

	return updates
}

// classifyUpdate determines if an update is patch, minor, or major.
// Versions are expected in semver format: vMAJOR.MINOR.PATCH
func classifyUpdate(current, latest string) string {
	cMaj, cMin, _ := parseSemver(current)
	lMaj, lMin, _ := parseSemver(latest)

	if cMaj != lMaj {
		return "major"
	}
	if cMin != lMin {
		return "minor"
	}
	return "patch"
}

// parseSemver extracts major, minor, patch from a Go module version string.
// Handles formats like v1.2.3, v1.2.3-pre, v0.0.0-date-hash (pseudo-versions).
func parseSemver(v string) (major, minor, patch string) {
	v = strings.TrimPrefix(v, "v")

	// Strip any pre-release suffix for comparison
	if idx := strings.Index(v, "-"); idx >= 0 {
		v = v[:idx]
	}

	parts := strings.SplitN(v, ".", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], "0"
	case 1:
		return parts[0], "0", "0"
	default:
		return "0", "0", "0"
	}
}

// createBeads creates bead issues for the outdated dependencies found in an anvil.
// Patch+minor updates go into a single auto-dispatch bead.
// Major updates go into a separate "needs attention" bead.
func (m *Monitor) createBeads(ctx context.Context, result CheckResult) {
	if len(result.Patch)+len(result.Minor) > 0 {
		m.createUpdateBead(ctx, result.Anvil, result.Path, "auto",
			append(result.Patch, result.Minor...))
	}

	if len(result.Major) > 0 {
		m.createUpdateBead(ctx, result.Anvil, result.Path, "major", result.Major)
	}
}

// createUpdateBead runs 'bd create' to create a bead for dependency updates.
func (m *Monitor) createUpdateBead(ctx context.Context, anvil, anvilPath, kind string, updates []ModuleUpdate) {
	var title string
	var priority string
	var desc strings.Builder

	switch kind {
	case "auto":
		title = fmt.Sprintf("Update %d Go dependencies (patch/minor)", len(updates))
		priority = "3"
		desc.WriteString("Automated dependency update for patch and minor version bumps.\n\n")
		desc.WriteString("Run `go get -u` for each module below, then `go mod tidy`.\n\n")
	case "major":
		title = fmt.Sprintf("Review %d Go major version updates", len(updates))
		priority = "2"
		desc.WriteString("Major version updates detected. These may contain breaking changes and require manual review.\n\n")
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
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] Failed to create bead for %s (%s): %v: %s", anvil, kind, err, stderr.String())
		_ = m.db.LogEvent(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to create %s update bead for %s: %v", kind, anvil, err), "", anvil)
		return
	}

	log.Printf("[depcheck] Created %s update bead for %s: %s", kind, anvil, strings.TrimSpace(string(output)))
	_ = m.db.LogEvent(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Created %s dependency update bead for %s (%d modules)", kind, anvil, len(updates)),
		"", anvil)
}
