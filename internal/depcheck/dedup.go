package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// BeadTitle returns the standardized bead title for a dependency update.
// Format: "Deps(<ecosystem>): update <package> <old> → <new>"
func BeadTitle(ecosystem, packageName, oldVersion, newVersion string) string {
	return fmt.Sprintf("Deps(%s): update %s %s → %s", ecosystem, packageName, oldVersion, newVersion)
}

// bdBead is a minimal struct for parsing bd list --json output.
type bdBead struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	ClosedAt    string `json:"closed_at"`
	UpdatedAt   string `json:"updated_at"`
}

// DedupCheck returns true if a bead for the given package already exists
// (open, in-progress, or recently closed within 7 days), or if an open PR
// references a bead that mentions the package. This prevents duplicate
// dependency update beads from accumulating.
//
// For checking many packages in a single cycle, prefer building a DedupCache
// with BuildDedupCache and calling DedupCheckWithCache to avoid per-package
// external-command fanout.
func DedupCheck(ctx context.Context, db *state.DB, anvilPath, anvilName, packageName, ecosystem string) bool {
	cache := BuildDedupCache(ctx, db, anvilPath, anvilName)
	found := DedupCheckWithCache(cache, packageName)
	if found {
		log.Printf("[depcheck] Dedup: found existing bead/PR for %s (%s) in %s", packageName, ecosystem, anvilName)
	}
	return found
}

// isRecentlyClosedBeadAt checks whether any bead in the slice mentions packageName
// and was closed after cutoff. Extracted for testability.
func isRecentlyClosedBeadAt(beads []bdBead, packageName string, cutoff time.Time) bool {
	for _, b := range beads {
		if !mentionsPackage(b, packageName) {
			continue
		}
		// Check if closed recently. Try closed_at first, fall back to updated_at.
		ts := b.ClosedAt
		if ts == "" {
			ts = b.UpdatedAt
		}
		if ts == "" {
			continue
		}
		t, err := parseBeadTime(ts)
		if err != nil {
			continue
		}
		if t.After(cutoff) {
			return true
		}
	}
	return false
}

// DedupCache holds pre-fetched bead and PR data for a single anvil.
// Build once per depcheck cycle with BuildDedupCache, then query cheaply
// with DedupCheckWithCache instead of spawning external commands per module.
type DedupCache struct {
	openRaw       []byte   // raw JSON from bd list --status=open
	inProgressRaw []byte   // raw JSON from bd list --status=in_progress
	closedBeads   []bdBead // parsed closed beads for time-based filtering
	closedRaw     []byte   // raw closed output for fallback string search
	prBeadOutputs [][]byte // raw JSON from bd show for each open PR's bead
	valid         bool     // true when the critical bd list commands succeeded
}

// BuildDedupCache fetches bead state once for an anvil, avoiding per-module
// external-command fanout in DedupCheckWithCache.
func BuildDedupCache(ctx context.Context, db *state.DB, anvilPath, anvilName string) *DedupCache {
	cache := &DedupCache{}

	var openErr, ipErr error
	cache.openRaw, openErr = fetchBeadList(ctx, anvilPath, "open")
	cache.inProgressRaw, ipErr = fetchBeadList(ctx, anvilPath, "in_progress")
	// The cache is only valid when the critical open/in_progress queries succeed.
	// If bd is unreachable, we must not create beads (would produce duplicates).
	cache.valid = openErr == nil && ipErr == nil

	closedRaw, _ := fetchBeadList(ctx, anvilPath, "closed")
	cache.closedRaw = closedRaw
	_ = json.Unmarshal(closedRaw, &cache.closedBeads) // best-effort; fallback handled in DedupCheckWithCache

	if db != nil {
		if prs, err := db.OpenPRs(); err == nil {
			for _, pr := range prs {
				if pr.Anvil != anvilName {
					continue
				}
				if out := fetchBeadShow(ctx, anvilPath, pr.BeadID); len(out) > 0 {
					cache.prBeadOutputs = append(cache.prBeadOutputs, out)
				}
			}
		}
	}

	return cache
}

// fetchBeadList runs bd list for the given status and returns raw output.
func fetchBeadList(ctx context.Context, anvilPath, status string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "list", fmt.Sprintf("--status=%s", status), "--limit", "0", "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] bd list --status=%s failed in %s: %v", status, anvilPath, err)
		return nil, err
	}
	return out, nil
}

// fetchBeadShow runs bd show for a single bead ID and returns raw output.
func fetchBeadShow(ctx context.Context, anvilPath, beadID string) []byte {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "show", beadID, "--json"))
	cmd.Dir = anvilPath
	out, _ := cmd.Output()
	return out
}

// DedupCheckWithCache checks whether a package already has an existing bead
// using pre-fetched data from BuildDedupCache.
func DedupCheckWithCache(cache *DedupCache, packageName string) bool {
	if containsPackageRef(cache.openRaw, packageName) {
		return true
	}
	if containsPackageRef(cache.inProgressRaw, packageName) {
		return true
	}
	for _, prOut := range cache.prBeadOutputs {
		if containsPackageRef(prOut, packageName) {
			return true
		}
	}
	// Check recently closed. If we have no parsed closed beads, treat this as
	// "unknown" rather than definitively suppressing new beads based on raw
	// output, to avoid disabling the 7-day recency window.
	if len(cache.closedBeads) == 0 {
		return false
	}
	cutoff := time.Now().AddDate(0, 0, -7)
	return isRecentlyClosedBeadAt(cache.closedBeads, packageName, cutoff)
}

// containsPackageRef checks if the bd JSON output references the package name.
// It first tries structured JSON parsing to match title/description fields,
// falling back to a simple string search.
func containsPackageRef(output []byte, packageName string) bool {
	var beads []bdBead
	if err := json.Unmarshal(output, &beads); err != nil {
		// bd show returns a single object, not an array.
		var single bdBead
		if err := json.Unmarshal(output, &single); err != nil {
			// Fall back to raw string search.
			return bytes.Contains(output, []byte(packageName))
		}
		return mentionsPackage(single, packageName)
	}

	for _, b := range beads {
		if mentionsPackage(b, packageName) {
			return true
		}
	}
	return false
}

// mentionsPackage checks if a bead's title or description refers to the package name.
func mentionsPackage(b bdBead, packageName string) bool {
	return textMentionsPackage(b.Title, packageName) || textMentionsPackage(b.Description, packageName)
}

// textMentionsPackage performs a more precise package-name match than a raw substring.
// It prefers the standardized bead title pattern
//
//	"Deps(<ecosystem>): update <package> <old> → <new>"
//
// and otherwise requires the package name to appear on clear word boundaries
// to avoid matching substrings of other package names (e.g., "react" in "react-dom").
func textMentionsPackage(text, packageName string) bool {
	if text == "" || packageName == "" {
		return false
	}

	// Prefer the standardized bead title format, which includes
	// " update <package> " for dependency updates.
	if strings.Contains(text, " update "+packageName+" ") {
		return true
	}

	// Fallback: require the package name to appear on space boundaries.
	if text == packageName {
		return true
	}
	if strings.HasPrefix(text, packageName+" ") {
		return true
	}
	if strings.HasSuffix(text, " "+packageName) {
		return true
	}
	if strings.Contains(text, " "+packageName+" ") {
		return true
	}

	return false
}

// parseBeadTime attempts to parse a timestamp from bd's JSON output.
// bd uses RFC3339 or similar formats.
func parseBeadTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unable to parse time: %s", s)
}
