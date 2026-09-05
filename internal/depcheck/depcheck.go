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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// DepsUpdateLabel is the label applied to dependency-update beads so that
// downstream consumers (e.g. worktree setup) can identify them and skip
// behaviours that conflict with npm install (such as node_modules junctions).
const DepsUpdateLabel = "deps-update"

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

// PreviewLivenessFunc reports the bead whose live Kiln preview currently holds
// the named anvil's checkout, or "" when the anvil has no live preview.
//
// A preview's worktree gets the main checkout's node_modules linked into it, so
// anything that deletes node_modules in the main checkout — `npm ci` does, as
// its first act — deletes it out from under the running preview. depcheck asks
// this before it touches an anvil's node_modules.
type PreviewLivenessFunc func(anvil string) string

// eventSink records one entry in the activity feed. It is the subset of
// state.DB the scanner writes events through, so the escalation path can be
// driven in a test without a SQLite file.
type eventSink interface {
	LogEvent(typ state.EventType, message, beadID, anvil string) error
}

// failureStore persists which blocking condition an anvil has already been
// escalated for. See state.DepcheckFailure for why the answer has to outlive
// the process: the whole point is that the NEXT run stays quiet.
type failureStore interface {
	RecordDepcheckFailure(f state.DepcheckFailure) (bool, error)
	ClearDepcheckFailure(anvil string) (bool, error)
	PruneDepcheckFailures(keep []string) error
}

// Scanner checks anvils for outdated dependencies across all supported ecosystems.
type Scanner struct {
	events      eventSink
	failures    failureStore
	owners      beadOwnerStore
	interval    time.Duration
	timeout     time.Duration
	anvilPaths  map[string]string // anvil name -> path
	previewLive PreviewLivenessFunc
	mu          sync.RWMutex
}

// minScanTimeout is the floor on the per-ecosystem scan timeout. It is a floor
// rather than a suggestion because Scanner.timeout is used as a DEADLINE, not
// as a descriptor: scanGo derives its context from it and the .NET and npm
// scans hand it to their own runners, so a zero duration is an already-expired
// deadline that kills `go list -m -u -json all` before it starts and reports
// every ecosystem as "context deadline exceeded".
const minScanTimeout = 1 * time.Minute

// New creates a dependency check scanner.
func New(db *state.DB, interval, timeout time.Duration, anvilPaths map[string]string) *Scanner {
	if interval < 1*time.Hour {
		interval = 1 * time.Hour
	}
	s := newScanner(db)
	s.interval = interval
	if timeout > s.timeout {
		s.timeout = timeout
	}
	s.anvilPaths = anvilPaths
	return s
}

// newScanner builds a scanner that can scan: its database-backed capabilities,
// and the timeout floor every ecosystem scan runs under. It is
// separate from New because the on-demand dispatch path builds a scanner
// without the periodic loop's interval and its configured timeout — but NOT
// without a usable timeout, which is why the floor is applied here rather than
// in New: a scanner built without one runs every ecosystem scan against an
// expired deadline.
//
// Both paths must also agree on the assignment below: a nil *state.DB stored in
// an interface is not a nil interface, so assigning it unguarded would defeat
// every nil check at the call sites and turn a scanner built without a database
// into a panic.
func newScanner(db *state.DB) *Scanner {
	s := &Scanner{timeout: minScanTimeout}
	if db != nil {
		s.events = db
		s.failures = db
		s.owners = db
	}
	return s
}

// rememberConsolidatedBead pins the bead as this anvil's, tolerating a scanner
// built without a database. Without the pin the next scan falls back to the
// description header, which is a weaker claim on the same bead.
func (s *Scanner) rememberConsolidatedBead(anvil, beadID string) {
	if s.owners == nil || beadID == "" {
		return
	}
	if err := s.owners.SetConsolidatedBead(anvil, beadID); err != nil {
		log.Printf("[depcheck] %s: could not record consolidated bead %s: %v", anvil, beadID, err)
	}
}

// emit records an event, tolerating a scanner built without a database (which
// is every unit test that drives a scan without one).
func (s *Scanner) emit(typ state.EventType, message, beadID, anvil string) {
	if s.events == nil {
		return
	}
	_ = s.events.LogEvent(typ, message, beadID, anvil)
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

// SetPreviewLiveness installs the callback the scanner consults before syncing
// an anvil's node_modules. Safe to call while Run is active. A nil callback
// (the default) means "no anvil ever has a live preview", which is exactly the
// behaviour of a Forge built without Kiln.
func (s *Scanner) SetPreviewLiveness(fn PreviewLivenessFunc) {
	s.mu.Lock()
	s.previewLive = fn
	s.mu.Unlock()
}

// previewHolder returns the bead whose live preview holds the anvil's checkout,
// or "" when there is none (including when no callback is installed).
func (s *Scanner) previewHolder(anvil string) string {
	s.mu.RLock()
	fn := s.previewLive
	s.mu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn(anvil)
}

// UpdateAnvilTags is a no-op kept for API compatibility. Consolidated dependency
// beads are no longer auto-tagged on creation; the user applies the anvil's
// configured auto-dispatch label (auto_dispatch_tag) manually when ready to dispatch the update.
func (s *Scanner) UpdateAnvilTags(_ map[string]string) {}

// Run starts the periodic check loop. Blocks until ctx is canceled.
func (s *Scanner) Run(ctx context.Context) error {
	log.Printf("[depcheck] Starting dependency checker (interval: %s, timeout: %s)", s.interval, s.timeout)
	s.emit(state.EventDepcheckStarted,
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

	s.pruneBlocked(anvils)

	for name, path := range anvils {
		if ctx.Err() != nil {
			return
		}
		s.scanAnvil(ctx, name, path)
	}
}

// ScanAnvilDeps runs all applicable ecosystem scanners for a single anvil and
// returns the results without creating beads. Unlike scanAnvil, it does not
// touch the remote — the caller should scan the working tree as-is.
func (s *Scanner) ScanAnvilDeps(ctx context.Context, name, path string) []*CheckResult {
	var results []*CheckResult
	for _, sc := range s.ecosystemScanners() {
		if ctx.Err() != nil {
			return results
		}
		result := sc.fn(ctx, name, path, worktreeSource{root: path})
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

// ecosystemScanner names one ecosystem's scan function. Each returns nil if the
// ecosystem is not present (e.g. no go.mod → scanGo returns nil).
type ecosystemScanner struct {
	name string
	fn   func(ctx context.Context, anvil, path string, src manifestSource) *CheckResult
}

func (s *Scanner) ecosystemScanners() []ecosystemScanner {
	return []ecosystemScanner{
		{"Go", s.scanGo},
		{"NuGet", s.scanDotnet},
		{"npm", s.scanNpm},
	}
}

// scanAnvil runs all applicable ecosystem scanners for a single anvil and
// creates beads for any outdated dependencies found.
//
// The manifests are read out of the anvil's upstream tracking ref, not out of
// its working tree. depcheck used to `git pull --ff-only` first, for the right
// reason — scanning a stale tree re-detects updates that upstream has already
// merged and files duplicate beads — but a pull is refused whenever a tracked
// file has local modifications the incoming commits touch, and some anvils are
// legitimately never clean (a pod-local `.beads/config.yaml`, bd's own
// additions to `.beads/.gitignore`). Such an anvil was skipped on that run and
// on every run after it, because both sides of the condition are permanent.
//
// A fetch cannot be refused by local modifications and writes only to .git, so
// the anvil is scanned with its modifications untouched and still carries them
// afterwards. The staleness the pull was guarding against is handled from the
// data instead: the ecosystem tools still run in the checkout, and what they
// report is reconciled against the versions the tracking ref actually pins.
func (s *Scanner) scanAnvil(ctx context.Context, name, path string) {
	src, err := s.refSourceFor(ctx, name, path)
	if err != nil {
		// The git plumbing is where the two failure classes live and where the
		// nightly-identical-event problem was observed, so this is the one path
		// that classifies. An ecosystem scan that fails below keeps reporting
		// per-run, since its errors are the ecosystem tool's rather than git's.
		s.reportScanFailure(ctx, name, path, err)
		return
	}
	// Reading the manifests is the proof that whatever blocked an earlier scan
	// is gone, so it is what withdraws the entry — no other path can know.
	s.clearBlocked(name)

	var allResults []*CheckResult
	for _, sc := range s.ecosystemScanners() {
		if ctx.Err() != nil {
			return
		}

		result := sc.fn(ctx, name, path, src)
		if result == nil {
			continue // ecosystem not present in this anvil
		}

		if result.Error != nil {
			log.Printf("[depcheck] Error checking %s (%s): %v", name, sc.name, result.Error)
			s.emit(state.EventDepcheckFailed,
				fmt.Sprintf("Dependency check failed for %s (%s): %v", name, sc.name, result.Error), "", name)
			continue
		}

		total := len(result.Patch) + len(result.Minor) + len(result.Major)
		if total == 0 {
			log.Printf("[depcheck] %s (%s): all dependencies up to date", name, sc.name)
			s.emit(state.EventDepcheckPassed,
				fmt.Sprintf("All %s dependencies up to date in %s", sc.name, name), "", name)
			continue
		}

		log.Printf("[depcheck] %s (%s): %d outdated (%d patch, %d minor, %d major)",
			name, sc.name, total, len(result.Patch), len(result.Minor), len(result.Major))
		s.emit(state.EventDepcheckFound,
			fmt.Sprintf("Found %d outdated %s dependencies in %s (%d patch, %d minor, %d major)",
				total, sc.name, name, len(result.Patch), len(result.Minor), len(result.Major)),
			"", name)

		allResults = append(allResults, result)
	}

	if len(allResults) > 0 {
		s.findOrCreateConsolidatedBead(ctx, allResults, path, name)
	}
}

// refSourceFor resolves the anvil's upstream tracking branch, fetches it, and
// returns the manifest source reading that ref's blobs.
func (s *Scanner) refSourceFor(ctx context.Context, name, path string) (manifestSource, error) {
	up, err := resolveUpstream(ctx, path)
	if err != nil {
		return nil, err
	}
	ref, err := fetchUpstream(ctx, path, up)
	if err != nil {
		return nil, err
	}
	log.Printf("[depcheck] %s: reading manifests from %s (working tree untouched)", name, ref)
	return refSource{repoDir: path, ref: ref}, nil
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
// It looks up this anvil's own open consolidated bead — not date-specific, so an
// open bead from a previous day is reused rather than duplicated, and not shared
// with another anvil in the same pool. If found, it appends any new packages to
// the description. If not found, it creates a new bead. The bead is intentionally
// left untagged; the user can apply the anvil's configured auto-dispatch label or
// workflow when they are ready to dispatch the update. (In auto_dispatch: all mode
// the bead is eligible immediately.)
func (s *Scanner) findOrCreateConsolidatedBead(ctx context.Context, allResults []*CheckResult, anvilPath, anvilName string) {
	title := consolidatedBeadTitle(time.Now())

	existing, err := findConsolidatedBead(ctx, s.owners, anvilPath, anvilName)
	if err != nil {
		log.Printf("[depcheck] %s: could not query existing beads — skipping bead creation: %v", anvilName, err)
		s.emit(state.EventDepcheckFailed,
			fmt.Sprintf("Skipped bead creation for %s — could not query existing beads: %v", anvilName, err), "", anvilName)
		return
	}

	if existing != nil {
		s.updateConsolidatedBead(ctx, existing, allResults, anvilPath, anvilName)
	} else {
		s.createConsolidatedBead(ctx, allResults, anvilPath, anvilName, title)
	}
}

// createConsolidatedBead creates a new consolidated dependency update bead
// containing all outdated packages from allResults. The bead is left untagged;
// the user can apply the anvil's configured auto-dispatch tag when they are
// ready to dispatch the update.
func (s *Scanner) createConsolidatedBead(ctx context.Context, allResults []*CheckResult, anvilPath, anvilName, title string) {
	desc := buildConsolidatedDescription(anvilName, allResults)

	// Use priority 2 if any major updates are present, otherwise 3.
	priority := "3"
	for _, r := range allResults {
		if r != nil && r.Error == nil && len(r.Major) > 0 {
			priority = "2"
			break
		}
	}

	cmd, cancel := executil.BdCommandTimeout(ctx, 3*time.Minute,
		"create",
		fmt.Sprintf("--title=%s", title),
		fmt.Sprintf("--description=%s", desc),
		"--type=chore",
		fmt.Sprintf("--priority=%s", priority),
		fmt.Sprintf("--labels=%s", DepsUpdateLabel),
		"--json",
	)
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] %s: failed to create consolidated bead: %v: %s", anvilName, err, stderr.String())
		s.emit(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to create consolidated dep bead for %s: %v", anvilName, err), "", anvilName)
		return
	}

	// Extract the bead ID from the JSON output for logging.
	// bd create --json may emit trailing diagnostics (e.g. orphan detection
	// warnings) after the JSON object; use DecodeJSON to tolerate the noise.
	var created struct {
		ID string `json:"id"`
	}
	_ = executil.DecodeJSON(output, &created)
	if created.ID != "" {
		s.rememberConsolidatedBead(anvilName, created.ID)
		log.Printf("[depcheck] %s: created consolidated bead %s", anvilName, created.ID)
		s.emit(state.EventDepcheckBeadCreated,
			fmt.Sprintf("Created consolidated dep bead for %s: %s", anvilName, created.ID), "", anvilName)
	} else {
		log.Printf("[depcheck] %s: created consolidated dep bead %q: %s", anvilName, title, strings.TrimSpace(string(output)))
		s.emit(state.EventDepcheckBeadCreated,
			fmt.Sprintf("Created consolidated dep bead for %s: %s", anvilName, title), "", anvilName)
	}
}

// updateConsolidatedBead merges new packages from allResults into the existing
// consolidated bead, rebuilding and updating the description.
func (s *Scanner) updateConsolidatedBead(ctx context.Context, existing *bdBead, allResults []*CheckResult, anvilPath, anvilName string) {
	existingAuto, existingMajor := parseConsolidatedDescription(existing.Description)
	mergedAuto, mergedMajor := mergeConsolidatedPackages(existingAuto, existingMajor, allResults)
	newDesc := buildDescriptionFromMaps(anvilName, mergedAuto, mergedMajor)

	// Check whether the label is already present on the bead.
	hasLabel := false
	for _, l := range existing.Labels {
		if strings.EqualFold(l, DepsUpdateLabel) {
			hasLabel = true
			break
		}
	}

	// Skip update if both description and label are already correct.
	if newDesc == existing.Description && hasLabel {
		log.Printf("[depcheck] %s: consolidated bead %s already up to date", anvilName, existing.ID)
		return
	}

	args := []string{"update", existing.ID, "--add-label", DepsUpdateLabel, "--json"}
	if newDesc != existing.Description {
		args = append(args, fmt.Sprintf("--description=%s", newDesc))
	}
	cmd, cancel := executil.BdCommandTimeout(ctx, 3*time.Minute, args...)
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] %s: failed to update consolidated bead %s: %v: %s", anvilName, existing.ID, err, stderr.String())
		s.emit(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to update consolidated dep bead for %s: %v", anvilName, err), "", anvilName)
		return
	}
	log.Printf("[depcheck] %s: updated consolidated bead %s: %s", anvilName, existing.ID, strings.TrimSpace(string(out)))
	s.emit(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Updated consolidated dep bead for %s: %s", anvilName, existing.ID), "", anvilName)
}
