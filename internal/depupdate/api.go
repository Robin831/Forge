package depupdate

import (
	"context"
	"fmt"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/state"
)

// Anvil describes a repository workspace for dependency scanning and updates.
type Anvil struct {
	// Name is the anvil's logical identifier (used in display and logging).
	Name string
	// Path is the absolute filesystem path to the repository root.
	Path string
	// Config carries per-anvil settings (temper flags, race detection, etc.).
	Config config.AnvilConfig
	// DB is used for event logging during scans. May be nil to disable logging.
	DB *state.DB
	// Timeout is the per-anvil scan timeout. Zero defaults to 5 minutes.
	Timeout time.Duration
}

// AnvilReport summarizes the available updates and pre-grouped changes for one
// anvil. Returned by Scan so callers (Hearth, Ledger) can display or apply
// updates without going through the CLI.
type AnvilReport struct {
	// Anvil is the source descriptor used when scanning.
	Anvil Anvil
	// Groups holds the update groups ready for Apply or Preview.
	Groups []UpdateGroup
	// Errors contains per-ecosystem scan errors (ecosystem name → error).
	// A nil map means all ecosystems scanned successfully.
	Errors map[string]error
}

// Result records the outcome of applying a single UpdateGroup.
type Result struct {
	// Group is the UpdateGroup that was processed.
	Group UpdateGroup
	// Applied is true when the group was installed, verified, and committed.
	Applied bool
	// Err holds the error that caused the group to be skipped or rolled back.
	// Nil when Applied is true.
	Err error
}

// Scan runs dependency checks across the given anvils and returns per-anvil
// reports with updates pre-grouped for display or application. Per-anvil scan
// failures are captured in AnvilReport.Errors rather than aborting the whole
// scan, so callers always receive a report for every requested anvil.
//
// This function provides the programmatic equivalent of `forge update-deps`
// without the interactive CLI layer, allowing Hearth and Ledger to query
// available updates directly.
func Scan(ctx context.Context, anvils []Anvil, opts Options) ([]AnvilReport, error) {
	var reports []AnvilReport
	for _, a := range anvils {
		if ctx.Err() != nil {
			break
		}
		timeout := a.Timeout
		if timeout == 0 {
			timeout = 5 * time.Minute
		}
		paths := map[string]string{a.Name: a.Path}
		scanner := depcheck.New(a.DB, time.Hour, timeout, paths)
		ecosystems := scanner.ScanAnvilDeps(ctx, a.Name, a.Path)

		errs := make(map[string]error)
		for _, cr := range ecosystems {
			if cr.Error != nil {
				errs[cr.Ecosystem] = cr.Error
			}
		}
		if len(errs) == 0 {
			errs = nil
		}

		groups := FilterGroups(GroupUpdates(ctx, ecosystems), opts)

		reports = append(reports, AnvilReport{
			Anvil:  a,
			Groups: groups,
			Errors: errs,
		})
	}
	return reports, nil
}

// Apply executes the given groups against the specified anvil by installing
// packages, verifying with Temper (build + lint + test), and committing on
// success or rolling back on failure. It returns one Result per input group.
//
// This function provides the programmatic equivalent of the apply phase in
// `forge update-deps --create-pr`, without the branch/PR/changelog steps.
// Callers (Hearth, Ledger) can wrap it with their own PR and changelog logic.
func Apply(ctx context.Context, anvilPath string, anvilCfg config.AnvilConfig, groups []UpdateGroup) ([]Result, error) {
	results := make([]Result, 0, len(groups))
	for _, g := range groups {
		if ctx.Err() != nil {
			results = append(results, Result{Group: g, Err: ctx.Err()})
			continue
		}
		if err := installGroup(ctx, anvilPath, g); err != nil {
			_ = RollbackGroup(ctx, anvilPath, g, err)
			results = append(results, Result{Group: g, Err: err})
			continue
		}
		tempered, verifyErr := VerifyGroup(ctx, anvilPath, anvilCfg)
		if verifyErr != nil {
			_ = RollbackGroup(ctx, anvilPath, g, verifyErr)
			results = append(results, Result{Group: g, Err: verifyErr})
			continue
		}
		if !tempered.Passed {
			failErr := fmt.Errorf("temper verification failed")
			_ = RollbackGroup(ctx, anvilPath, g, failErr)
			results = append(results, Result{Group: g, Err: failErr})
			continue
		}
		if err := CommitGroup(ctx, anvilPath, g); err != nil {
			_ = RollbackGroup(ctx, anvilPath, g, err)
			results = append(results, Result{Group: g, Err: err})
			continue
		}
		results = append(results, Result{Group: g, Applied: true})
	}
	return results, nil
}

// Preview returns a Result slice describing what Apply would do for the given
// groups without making any changes to disk or running any package-manager
// commands. It is the dry-run variant used by Hearth's overlay preview so
// users can inspect pending updates before confirming application.
func Preview(groups []UpdateGroup) []Result {
	results := make([]Result, len(groups))
	for i, g := range groups {
		results[i] = Result{Group: g}
	}
	return results
}
