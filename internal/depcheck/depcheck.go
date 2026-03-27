// Package depcheck periodically checks registered anvils for outdated
// dependencies, starting with Go and designed to support additional ecosystems
// (.NET, npm) in the future. When updates are found it creates beads so a
// Smith agent can apply them. Patch/minor updates produce auto-dispatch beads;
// major version bumps produce "needs attention" beads.
package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// ModuleUpdate describes a single outdated dependency.
type ModuleUpdate struct {
	Path      string // module/package path
	Current   string // current version
	Latest    string // latest available version
	Kind      string // "patch", "minor", or "major"
	SourceDir string // directory containing the manifest file (e.g. package.json)
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
	anvilTags  map[string]string // anvil name -> auto-dispatch label (e.g. "forgeReady")
	mu         sync.RWMutex
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

// UpdateAnvilPaths replaces the set of anvils to scan. This is safe to call
// while Run is active and takes effect on the next scan cycle.
func (s *Scanner) UpdateAnvilPaths(paths map[string]string) {
	copied := make(map[string]string, len(paths))
	for k, v := range paths {
		copied[k] = v
	}
	s.mu.Lock()
	s.anvilPaths = copied
	s.mu.Unlock()
}

// UpdateAnvilTags replaces the per-anvil auto-dispatch label map. Tags are used
// when creating consolidated dependency beads so that each anvil uses its own
// configured label (e.g. "forgeReady", "forge-auto"). This is safe to call
// while Run is active and takes effect on the next scan cycle.
func (s *Scanner) UpdateAnvilTags(tags map[string]string) {
	copied := make(map[string]string, len(tags))
	for k, v := range tags {
		copied[k] = v
	}
	s.mu.Lock()
	s.anvilTags = copied
	s.mu.Unlock()
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
	s.mu.RLock()
	anvils := make(map[string]string, len(s.anvilPaths))
	for k, v := range s.anvilPaths {
		anvils[k] = v
	}
	s.mu.RUnlock()

	log.Printf("[depcheck] Checking %d anvils for outdated dependencies", len(anvils))

	for name, path := range anvils {
		if ctx.Err() != nil {
			return
		}
		s.scanAnvil(ctx, name, path)
	}
}

// ScanAnvilDeps runs all applicable ecosystem scanners for a single anvil and
// returns the results without creating beads. Unlike scanAnvil, it does not
// pull from the remote — the caller should scan the working tree as-is.
func (s *Scanner) ScanAnvilDeps(ctx context.Context, name, path string) []*CheckResult {
	scanners := []struct {
		name string
		fn   func(ctx context.Context, anvil, path string) *CheckResult
	}{
		{"Go", s.scanGo},
		{"NuGet", s.scanDotnet},
		{"npm", s.scanNpm},
	}

	var results []*CheckResult
	for _, sc := range scanners {
		if ctx.Err() != nil {
			return results
		}
		result := sc.fn(ctx, name, path)
		if result == nil {
			continue // ecosystem not present
		}
		if result.Error != nil {
			log.Printf("[depcheck] Error checking %s (%s): %v", name, sc.name, result.Error)
			// Return the errored result so the CLI can display the failure rather
			// than silently treating the anvil as up to date.
		}
		results = append(results, result)
	}
	return results
}

// scanAnvil runs all applicable ecosystem scanners for a single anvil and
// creates beads for any outdated dependencies found.
func (s *Scanner) scanAnvil(ctx context.Context, name, path string) {
	// Pull latest main so the scanner sees current dependency versions.
	// Without this, merged dependency updates that haven't been pulled
	// locally would be re-detected as outdated, creating duplicate beads.
	// If the pull fails we must not scan — scanning a stale tree would
	// produce beads for work that is already done.
	pullCtx, pullCancel := context.WithTimeout(ctx, 30*time.Second)
	defer pullCancel()
	pullCmd := executil.HideWindow(exec.CommandContext(pullCtx, "git", "pull", "--ff-only"))
	pullCmd.Dir = path
	if out, pullErr := pullCmd.CombinedOutput(); pullErr != nil {
		msg := fmt.Sprintf("git pull --ff-only failed for anvil %s — skipping depcheck to avoid stale results: %v: %s",
			name, pullErr, strings.TrimSpace(string(out)))
		log.Printf("[depcheck] %s", msg)
		_ = s.db.LogEvent(state.EventDepcheckFailed, msg, "", name)
		return
	}

	// Run each ecosystem scanner. Each returns nil if the ecosystem is not
	// present (e.g. no go.mod → scanGo returns nil).
	scanners := []struct {
		name string
		fn   func(ctx context.Context, anvil, path string) *CheckResult
	}{
		{"Go", s.scanGo},
		{"NuGet", s.scanDotnet},
		{"npm", s.scanNpm},
	}

	var allResults []*CheckResult
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

		allResults = append(allResults, result)
	}

	if len(allResults) > 0 {
		s.mu.RLock()
		autoDispatchTag := s.anvilTags[name]
		s.mu.RUnlock()
		s.findOrCreateConsolidatedBead(ctx, allResults, path, name, autoDispatchTag)
	}
}

// sortUpdates sorts ModuleUpdate slices by kind (major first) then path.
// Used by all ecosystem scanners for consistent output ordering.
func sortUpdates(updates []ModuleUpdate) {
	order := map[string]int{"major": 0, "minor": 1, "patch": 2}
	sort.Slice(updates, func(i, j int) bool {
		if updates[i].Kind != updates[j].Kind {
			return order[updates[i].Kind] < order[updates[j].Kind]
		}
		return updates[i].Path < updates[j].Path
	})
}

// classifyUpdate determines if an update is patch, minor, or major.
// Versions may use semver (vMAJOR.MINOR.PATCH) or bare numeric formats
// (e.g. npm uses "1.2.3" without the v prefix; .NET uses "1.2.3").
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

// parseSemver extracts major, minor, patch from a version string.
// Handles formats like v1.2.3, 1.2.3, v1.2.3-pre, and
// v0.0.0-date-hash (Go pseudo-versions).
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

// findOrCreateConsolidatedBead is the main bead management entry point.
// It searches for an existing open bead with today's consolidated title for this anvil.
// If found, it appends any new packages to the description.
// If not found, it creates a new bead tagged with autoDispatchTag.
func (s *Scanner) findOrCreateConsolidatedBead(ctx context.Context, allResults []*CheckResult, anvilPath, anvilName, autoDispatchTag string) {
	title := consolidatedBeadTitle(time.Now())

	existing, err := findConsolidatedBead(ctx, anvilPath, title)
	if err != nil {
		log.Printf("[depcheck] %s: could not query existing beads — skipping bead creation: %v", anvilName, err)
		_ = s.db.LogEvent(state.EventDepcheckFailed,
			fmt.Sprintf("Skipped bead creation for %s — could not query existing beads: %v", anvilName, err), "", anvilName)
		return
	}

	if existing != nil {
		s.updateConsolidatedBead(ctx, existing, allResults, anvilPath, anvilName)
	} else {
		s.createConsolidatedBead(ctx, allResults, anvilPath, anvilName, title, autoDispatchTag)
	}
}

// createConsolidatedBead creates a new consolidated dependency update bead
// containing all outdated packages from allResults, then tags it with autoDispatchTag.
func (s *Scanner) createConsolidatedBead(ctx context.Context, allResults []*CheckResult, anvilPath, anvilName, title, autoDispatchTag string) {
	desc := buildConsolidatedDescription(anvilName, allResults)

	// Use priority 2 if any major updates are present, otherwise 3.
	priority := "3"
	for _, r := range allResults {
		if r != nil && r.Error == nil && len(r.Major) > 0 {
			priority = "2"
			break
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "create",
		fmt.Sprintf("--title=%s", title),
		fmt.Sprintf("--description=%s", desc),
		"--type=chore",
		fmt.Sprintf("--priority=%s", priority),
		"--json",
	))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] %s: failed to create consolidated bead: %v: %s", anvilName, err, stderr.String())
		_ = s.db.LogEvent(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to create consolidated dep bead for %s: %v", anvilName, err), "", anvilName)
		return
	}

	log.Printf("[depcheck] %s: created consolidated dep bead %q: %s", anvilName, title, strings.TrimSpace(string(output)))
	_ = s.db.LogEvent(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Created consolidated dep bead for %s: %s", anvilName, title), "", anvilName)

	// Extract the bead ID from the JSON output so we can tag it for auto-dispatch.
	// bd create --json may emit extra lines (e.g. progress messages) before the JSON
	// object, so fall back to scanning each line if a whole-output unmarshal fails.
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &created); err != nil || created.ID == "" {
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if err2 := json.Unmarshal([]byte(line), &created); err2 == nil && created.ID != "" {
				break
			}
		}
	}
	if created.ID != "" && autoDispatchTag != "" {
		s.addForgeReadyLabel(ctx, created.ID, anvilPath, anvilName, autoDispatchTag)
	}
}

// updateConsolidatedBead merges new packages from allResults into the existing
// consolidated bead, rebuilding and updating the description.
func (s *Scanner) updateConsolidatedBead(ctx context.Context, existing *bdBead, allResults []*CheckResult, anvilPath, anvilName string) {
	existingAuto, existingMajor := parseConsolidatedDescription(existing.Description)
	mergedAuto, mergedMajor := mergeConsolidatedPackages(existingAuto, existingMajor, allResults)
	newDesc := buildDescriptionFromMaps(anvilName, mergedAuto, mergedMajor)

	// Skip update if description is unchanged.
	if newDesc == existing.Description {
		log.Printf("[depcheck] %s: consolidated bead %s already up to date", anvilName, existing.ID)
		return
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "update", existing.ID,
		fmt.Sprintf("--description=%s", newDesc),
		"--json",
	))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] %s: failed to update consolidated bead %s: %v: %s", anvilName, existing.ID, err, stderr.String())
		_ = s.db.LogEvent(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to update consolidated dep bead for %s: %v", anvilName, err), "", anvilName)
		return
	}
	log.Printf("[depcheck] %s: updated consolidated bead %s: %s", anvilName, existing.ID, strings.TrimSpace(string(out)))
	_ = s.db.LogEvent(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Updated consolidated dep bead for %s: %s", anvilName, existing.ID), "", anvilName)
}

// addForgeReadyLabel tags a bead with the given label so the poller picks it up.
func (s *Scanner) addForgeReadyLabel(ctx context.Context, beadID, anvilPath, anvilName, label string) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "update", beadID, "--add-label", label, "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[depcheck] %s: failed to add %s label to %s: %v: %s", anvilName, label, beadID, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("[depcheck] %s: tagged %s with %s: %s", anvilName, beadID, label, strings.TrimSpace(string(out)))
}
