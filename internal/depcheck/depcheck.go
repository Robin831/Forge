// Package depcheck periodically checks registered anvils for outdated dependencies
// across multiple ecosystems (Go, .NET/NuGet, npm). When updates are found, it
// creates beads so a Smith agent can apply the updates. Patch/minor updates are
// grouped into a single bead per ecosystem; major version bumps get individual beads.
package depcheck

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// Ecosystem identifies the package manager that produced an update.
type Ecosystem string

const (
	EcosystemGo     Ecosystem = "Go"
	EcosystemDotnet Ecosystem = "NuGet"
	EcosystemNpm    Ecosystem = "npm"
)

// DependencyUpdate describes an outdated dependency.
type DependencyUpdate struct {
	Ecosystem Ecosystem // Go, NuGet, npm
	Package   string    // module/package name
	Current   string    // current version
	Latest    string    // latest available version
	Kind      string    // "patch", "minor", or "major"
	Subdir    string    // subdirectory within the anvil (empty for root)
}

// ScanResult holds the depcheck results for a single anvil.
type ScanResult struct {
	Anvil   string
	Path    string
	Updates []DependencyUpdate
	Error   error
	Checked time.Time
}

// UpdatesByKind groups updates into patch+minor and major buckets.
func (r ScanResult) UpdatesByKind() (patchMinor, major []DependencyUpdate) {
	for _, u := range r.Updates {
		if u.Kind == "major" {
			major = append(major, u)
		} else {
			patchMinor = append(patchMinor, u)
		}
	}
	return
}

// UpdatesByEcosystem groups updates by ecosystem.
func (r ScanResult) UpdatesByEcosystem() map[Ecosystem][]DependencyUpdate {
	m := make(map[Ecosystem][]DependencyUpdate)
	for _, u := range r.Updates {
		m[u.Ecosystem] = append(m[u.Ecosystem], u)
	}
	return m
}

// Monitor periodically checks anvils for outdated dependencies across ecosystems.
type Monitor struct {
	db         *state.DB
	interval   time.Duration
	timeout    time.Duration
	anvilPaths map[string]string // anvil name -> path
	disabled   map[string]bool   // anvil name -> depcheck_enabled=false
}

// New creates a dependency check monitor.
// disabled maps anvil names to true when depcheck_enabled is explicitly false.
func New(db *state.DB, interval, timeout time.Duration, anvilPaths map[string]string, disabled map[string]bool) *Monitor {
	if interval < 1*time.Hour {
		interval = 1 * time.Hour
	}
	if timeout < 1*time.Minute {
		timeout = 1 * time.Minute
	}
	if disabled == nil {
		disabled = make(map[string]bool)
	}
	return &Monitor{
		db:         db,
		interval:   interval,
		timeout:    timeout,
		anvilPaths: anvilPaths,
		disabled:   disabled,
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

// CheckAll runs dependency checks on all anvils and returns results (for CLI use).
func (m *Monitor) CheckAll(ctx context.Context) []ScanResult {
	return m.checkAllResults(ctx)
}

// checkAll runs dependency checks on all anvils (for scheduled use, creates beads).
func (m *Monitor) checkAll(ctx context.Context) {
	log.Printf("[depcheck] Checking %d anvils for outdated dependencies", len(m.anvilPaths))

	for name, path := range m.anvilPaths {
		if ctx.Err() != nil {
			return
		}

		if m.disabled[name] {
			log.Printf("[depcheck] %s: skipped (depcheck_enabled: false)", name)
			continue
		}

		result := m.checkAnvil(ctx, name, path)
		if result.Error != nil {
			log.Printf("[depcheck] Error checking %s: %v", name, result.Error)
			_ = m.db.LogEvent(state.EventDepcheckFailed,
				fmt.Sprintf("Dependency check failed for %s: %v", name, result.Error), "", name)
			continue
		}

		if len(result.Updates) == 0 {
			log.Printf("[depcheck] %s: all dependencies up to date", name)
			_ = m.db.LogEvent(state.EventDepcheckPassed,
				fmt.Sprintf("All dependencies up to date in %s", name), "", name)
			continue
		}

		byEco := result.UpdatesByEcosystem()
		var parts []string
		for eco, ups := range byEco {
			parts = append(parts, fmt.Sprintf("%d %s", len(ups), eco))
		}
		sort.Strings(parts)
		log.Printf("[depcheck] %s: %d outdated (%s)",
			name, len(result.Updates), strings.Join(parts, ", "))
		_ = m.db.LogEvent(state.EventDepcheckFound,
			fmt.Sprintf("Found %d outdated dependencies in %s (%s)",
				len(result.Updates), name, strings.Join(parts, ", ")),
			"", name)

		m.createBeads(ctx, result)
	}
}

// checkAllResults is like checkAll but returns results instead of creating beads.
func (m *Monitor) checkAllResults(ctx context.Context) []ScanResult {
	var results []ScanResult
	for name, path := range m.anvilPaths {
		if ctx.Err() != nil {
			break
		}
		if m.disabled[name] {
			continue
		}
		results = append(results, m.checkAnvil(ctx, name, path))
	}
	return results
}

// checkAnvil discovers and scans all ecosystems present in an anvil.
func (m *Monitor) checkAnvil(ctx context.Context, name, path string) ScanResult {
	result := ScanResult{
		Anvil:   name,
		Path:    path,
		Checked: time.Now(),
	}

	var allUpdates []DependencyUpdate

	// Go: check for go.mod at root
	if goUpdates, err := scanGo(ctx, path, m.timeout); err != nil {
		log.Printf("[depcheck] %s: Go scan error: %v", name, err)
	} else {
		allUpdates = append(allUpdates, goUpdates...)
	}

	// .NET: walk for *.csproj and *.sln files
	dotnetRoots := findDotnetRoots(path)
	for _, root := range dotnetRoots {
		subdir := relativeSubdir(path, root)
		if updates, err := scanDotnet(ctx, root, subdir, m.timeout); err != nil {
			log.Printf("[depcheck] %s: .NET scan error in %s: %v", name, subdir, err)
		} else {
			allUpdates = append(allUpdates, updates...)
		}
	}

	// npm: walk for package.json files (skip node_modules)
	npmRoots := findNpmRoots(path)
	for _, root := range npmRoots {
		subdir := relativeSubdir(path, root)
		if updates, err := scanNpm(ctx, root, subdir, m.timeout); err != nil {
			log.Printf("[depcheck] %s: npm scan error in %s: %v", name, subdir, err)
		} else {
			allUpdates = append(allUpdates, updates...)
		}
	}

	// Sort: major first, then minor, then patch; within same kind by package name
	order := map[string]int{"major": 0, "minor": 1, "patch": 2}
	sort.Slice(allUpdates, func(i, j int) bool {
		if allUpdates[i].Kind != allUpdates[j].Kind {
			return order[allUpdates[i].Kind] < order[allUpdates[j].Kind]
		}
		if allUpdates[i].Ecosystem != allUpdates[j].Ecosystem {
			return allUpdates[i].Ecosystem < allUpdates[j].Ecosystem
		}
		return allUpdates[i].Package < allUpdates[j].Package
	})

	result.Updates = allUpdates
	return result
}

// relativeSubdir returns the relative path from base to dir, or "." if they're the same.
func relativeSubdir(base, dir string) string {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// findDotnetRoots finds directories containing *.sln or *.csproj files.
// Returns unique directory paths, preferring sln directories over csproj.
func findDotnetRoots(root string) []string {
	seen := make(map[string]bool)
	var roots []string

	// Look for .sln files first (solution-level)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == "node_modules" || name == ".git" || name == "bin" || name == "obj" || name == ".workers" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".sln") {
			dir := filepath.Dir(path)
			if !seen[dir] {
				seen[dir] = true
				roots = append(roots, dir)
			}
		}
		return nil
	})

	// If no .sln found, look for .csproj files
	if len(roots) == 0 {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				name := info.Name()
				if name == "node_modules" || name == ".git" || name == "bin" || name == "obj" || name == ".workers" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".csproj") {
				dir := filepath.Dir(path)
				if !seen[dir] {
					seen[dir] = true
					roots = append(roots, dir)
				}
			}
			return nil
		})
	}

	return roots
}

// findNpmRoots finds directories containing package.json (skipping node_modules).
func findNpmRoots(root string) []string {
	var roots []string

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == "node_modules" || name == ".git" || name == ".workers" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() == "package.json" {
			roots = append(roots, filepath.Dir(path))
		}
		return nil
	})

	return roots
}

// classifyUpdate determines if an update is patch, minor, or major.
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
// Handles formats like v1.2.3, 1.2.3, v1.2.3-pre, 0.0.0-date-hash.
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
