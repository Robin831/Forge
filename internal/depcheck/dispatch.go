package depcheck

import (
	"context"
	"fmt"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// FindOrCreateBeadID locates the given anvil's open consolidated dependency-update
// bead, running a fresh scan and creating the bead if it has none yet. An existing
// bead from a previous day is reused rather than creating a new one each time the
// date changes, and a bead belonging to another anvil in the same pool is never
// returned.
//
// Returns:
//   - (beadID, nil)  when a bead was found or created successfully
//   - ("", nil)      when no outdated packages were detected (nothing to dispatch)
//   - ("", err)      when the scan or bead-creation step failed
func FindOrCreateBeadID(ctx context.Context, db *state.DB, anvilName, anvilPath string) (string, error) {
	title := consolidatedBeadTitle(time.Now())
	s := newScanner(db)

	// Fast path: this anvil already has an open consolidated bead.
	existing, err := findConsolidatedBead(ctx, s.owners, anvilPath, anvilName)
	if err != nil {
		return "", fmt.Errorf("querying existing bead: %w", err)
	}
	if existing != nil {
		return existing.ID, nil
	}

	// No bead yet — run the dependency scan to find outdated packages.
	results := s.ScanAnvilDeps(ctx, anvilName, anvilPath)

	var firstScanErr error
	successfulScan := false
	hasUpdates := false
	for _, r := range results {
		if r == nil {
			continue
		}
		if r.Error != nil {
			if firstScanErr == nil {
				firstScanErr = fmt.Errorf("dependency scan failed: %w", r.Error)
			}
			continue
		}
		successfulScan = true
		if len(r.Patch) > 0 || len(r.Minor) > 0 || len(r.Major) > 0 {
			hasUpdates = true
		}
	}
	if !hasUpdates {
		// If no successful scans ran but at least one scanner failed, surface
		// an error instead of reporting "nothing to update".
		if !successfulScan && firstScanErr != nil {
			return "", firstScanErr
		}
		return "", nil // nothing to update
	}

	// Create the consolidated bead.
	s.createConsolidatedBead(ctx, results, anvilPath, anvilName, title)

	// Re-query to retrieve the newly created bead's ID.
	created, err := findConsolidatedBead(ctx, s.owners, anvilPath, anvilName)
	if err != nil {
		return "", fmt.Errorf("querying created bead: %w", err)
	}
	if created == nil {
		return "", fmt.Errorf("no consolidated bead found after creation attempt — creation may have failed or bd sync may be delayed")
	}
	return created.ID, nil
}
