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
func DedupCheck(ctx context.Context, db *state.DB, anvilPath, anvilName, packageName, ecosystem string) bool {
	// 1. Check open and in-progress beads.
	if found := beadExistsInStatus(ctx, anvilPath, packageName, "open"); found {
		log.Printf("[depcheck] Dedup: found open bead for %s in %s", packageName, anvilName)
		return true
	}
	if found := beadExistsInStatus(ctx, anvilPath, packageName, "in_progress"); found {
		log.Printf("[depcheck] Dedup: found in-progress bead for %s in %s", packageName, anvilName)
		return true
	}

	// 2. Check open PRs in state DB — their associated beads may reference the package.
	if found := prBeadReferencesPackage(ctx, db, anvilPath, anvilName, packageName); found {
		log.Printf("[depcheck] Dedup: found open PR bead for %s in %s", packageName, anvilName)
		return true
	}

	// 3. Check recently closed beads (last 7 days).
	if found := recentlyClosedBeadExists(ctx, anvilPath, packageName); found {
		log.Printf("[depcheck] Dedup: found recently closed bead for %s in %s", packageName, anvilName)
		return true
	}

	return false
}

// beadExistsInStatus checks whether any bead with the given status mentions
// the package name in its title or description.
func beadExistsInStatus(ctx context.Context, anvilPath, packageName, status string) bool {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "list", fmt.Sprintf("--status=%s", status), "--json"))
	cmd.Dir = anvilPath

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	return containsPackageRef(output, packageName)
}

// prBeadReferencesPackage checks if any open PR's associated bead mentions the package.
// It fetches open PRs from the state DB, then queries bd show for each bead.
func prBeadReferencesPackage(ctx context.Context, db *state.DB, anvilPath, anvilName, packageName string) bool {
	if db == nil {
		return false
	}

	prs, err := db.OpenPRs()
	if err != nil {
		return false
	}

	for _, pr := range prs {
		if pr.Anvil != anvilName {
			continue
		}
		// Query the bead details for this PR's bead ID.
		cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
			"bd", "show", pr.BeadID, "--json"))
		cmd.Dir = anvilPath

		output, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}

		if containsPackageRef(output, packageName) {
			return true
		}
	}

	return false
}

// recentlyClosedBeadExists checks if a bead mentioning the package was closed
// within the last 7 days.
func recentlyClosedBeadExists(ctx context.Context, anvilPath, packageName string) bool {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "list", "--status=closed", "--json"))
	cmd.Dir = anvilPath

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	var beads []bdBead
	if err := json.Unmarshal(output, &beads); err != nil {
		// If JSON parsing fails, fall back to simple string search.
		return false
	}

	cutoff := time.Now().AddDate(0, 0, -7)
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

// mentionsPackage checks if a bead's title or description contains the package name.
func mentionsPackage(b bdBead, packageName string) bool {
	return strings.Contains(b.Title, packageName) || strings.Contains(b.Description, packageName)
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
