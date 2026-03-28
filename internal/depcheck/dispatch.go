package depcheck

import (
	"context"
	"fmt"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// FindOrCreateBeadID locates today's consolidated dependency-update bead for the
// given anvil, running a fresh scan and creating the bead if none exists yet.
//
// Returns:
//   - (beadID, nil)  when a bead was found or created successfully
//   - ("", nil)      when no outdated packages were detected (nothing to dispatch)
//   - ("", err)      when the scan or bead-creation step failed
func FindOrCreateBeadID(ctx context.Context, db *state.DB, anvilName, anvilPath string) (string, error) {
	title := consolidatedBeadTitle(time.Now())

	// Fast path: bead already exists for today.
	existing, err := findConsolidatedBead(ctx, anvilPath, title)
	if err != nil {
		return "", fmt.Errorf("querying existing bead: %w", err)
	}
	if existing != nil {
		return existing.ID, nil
	}

	// No bead yet — run the dependency scan to find outdated packages.
	s := &Scanner{db: db}
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
	created, err := findConsolidatedBead(ctx, anvilPath, title)
	if err != nil {
		return "", fmt.Errorf("querying created bead: %w", err)
	}
	if created == nil {
		return "", fmt.Errorf("no consolidated bead found after creation attempt — creation may have failed or bd sync may be delayed")
	}
	return created.ID, nil
}
