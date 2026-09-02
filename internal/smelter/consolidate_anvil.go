package smelter

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Robin831/Forge/internal/warden"
)

// ConsolidateOptions configures a single off-cycle three-pass consolidation
// run against the warden rules file at AnvilPath. The same option set is
// used by the scheduled smelter loop and by the `forge warden consolidate`
// CLI command so both code paths share one implementation.
type ConsolidateOptions struct {
	// AnvilPath is the on-disk path to the anvil root. Both
	// .forge/warden-rules.yaml and .forge/warden-rules.archive.yaml are
	// resolved relative to it.
	AnvilPath string
	// AnvilName is used in log messages and event payloads.
	AnvilName string
	// Consolidator is the AI invocation hook used by Pass 1. When nil, Pass
	// 1 is skipped.
	Consolidator warden.ConsolidationRunner
	// DedupThreshold is the Jaccard similarity threshold (0.0–1.0) above
	// which two rules in the same category are considered duplicates. When
	// <= 0, Pass 1 is skipped.
	DedupThreshold float64
	// OverlapThreshold is the second near-duplicate criterion (see
	// warden.DedupParams). Zero falls back to
	// warden.DefaultOverlapThreshold; a negative value disables it, leaving
	// Jaccard as the only test. It never enables Pass 1 on its own —
	// DedupThreshold <= 0 still skips the pass entirely.
	OverlapThreshold float64
	// ArchiveAfterDays is the staleness threshold in days for Pass 2. When
	// <= 0, Pass 2 is skipped.
	ArchiveAfterDays int
	// MaxRulesInFile is the hard ceiling on the active rules file. When <= 0,
	// the eviction pass is skipped.
	MaxRulesInFile int
	// Now optionally overrides the reference time used by the staleness
	// pass. Zero defaults to time.Now().UTC().
	Now time.Time
	// EventLogger is an optional callback for surfacing per-pass progress
	// to an external sink. The scheduled smelter wires this to the state
	// DB; CLI invocations may pass nil. Called with (eventName, message).
	EventLogger func(name, message string)
}

// ConsolidateResult bundles the structured output of a three-pass run.
// Callers (smelter loop / CLI) use it to render a summary and (in the CLI
// case) decide whether to print "no changes".
type ConsolidateResult struct {
	// Passes captures the per-pass outcomes in the standard PassResults
	// shape so the same commit-message and summary helpers work for both
	// scheduled and off-cycle runs.
	Passes PassResults
	// Pass1Archived lists the original rules superseded by Pass 1 merges.
	// Surfaced so callers that drive the smelter flushAnvil path (which
	// writes them to the archive store) can reuse the same result.
	Pass1Archived []warden.Rule
	// InitialCount is the rule count loaded from the active file before
	// any pass ran.
	InitialCount int
	// FinalActive is the rule count in the active file after all passes
	// completed and persistence (when any pass produced changes) ran.
	FinalActive int
	// ArchiveCount is the entry count in the archive file after the run.
	// Zero when no archive file exists.
	ArchiveCount int
	// FirstError is the first non-fatal error encountered during Pass 1
	// (typically an AI runner failure for a single cluster). The run
	// proceeds despite this; callers may choose to surface it in their
	// summary.
	FirstError error
}

// ConsolidateAnvil loads the warden rules file under opts.AnvilPath, runs
// the three smelter passes (Pass 1 cluster consolidation, Pass 2 staleness
// archive, Pass 3 paths backfill) against the in-memory rules, and persists
// the results when any pass produced changes. Idempotent: rerunning against
// an already-consolidated file is a no-op (Passes.HasChanges() will be
// false in the result).
//
// Pass 1 is skipped when opts.Consolidator is nil or opts.DedupThreshold
// <= 0. Pass 2 is skipped when opts.ArchiveAfterDays <= 0. Pass 3 has no
// threshold and runs over rules whose Source carries a copilot:PR#N token
// and whose Paths field is empty (idempotent on already-backfilled rules).
//
// Behaviour notes:
//   - The active rules file is always written when any pass produced changes.
//   - The archive file is written only when Pass 1 (duplicates) or Pass 2
//     (stale rules) produced entries to archive. A Pass 3-only run updates
//     the active rules file but leaves the archive file untouched.
//   - When the archive is written, it lands before the active rules file so
//     a partial failure can never leave the active file without a matching
//     archive record.
//   - The pending warden rules queue in state.db is NOT consulted —
//     ConsolidateAnvil operates only on what is already on disk. Pulling
//     pending rules into the active file remains the smelter loop's
//     responsibility.
//   - When opts.Consolidator returns errors for individual clusters they
//     are aggregated and the first one is reported in the result; the
//     remaining clusters still merge.
func ConsolidateAnvil(ctx context.Context, opts ConsolidateOptions) (ConsolidateResult, error) {
	rf, err := warden.LoadRules(opts.AnvilPath)
	if err != nil {
		return ConsolidateResult{}, fmt.Errorf("loading warden rules: %w", err)
	}
	initialCount := len(rf.Rules)

	var (
		summary      []warden.MergeResult
		replaced     []warden.Rule
		firstPassErr error
	)
	if opts.Consolidator != nil && opts.DedupThreshold > 0 {
		var errs []error
		overlap := opts.OverlapThreshold
		if overlap == 0 {
			overlap = warden.DefaultOverlapThreshold
		}
		params := warden.DedupParams{Jaccard: opts.DedupThreshold, Overlap: overlap}
		replaced, summary, errs = warden.ConsolidateWithParams(ctx, opts.AnvilPath, rf, params, opts.Consolidator)
		for i, e := range errs {
			if i == 0 {
				firstPassErr = e
				continue
			}
			log.Printf("[smelter] additional consolidation error for %s: %v", opts.AnvilName, e)
		}
		if len(summary) > 0 {
			log.Printf("[smelter] consolidated %d cluster(s) for %s", len(summary), opts.AnvilName)
			if opts.EventLogger != nil {
				opts.EventLogger("smelter_flushed",
					fmt.Sprintf("Consolidated %d cluster(s) for %s", len(summary), opts.AnvilName))
			}
		}
	}

	var stale []warden.ArchivedRule
	if opts.ArchiveAfterDays > 0 {
		now := opts.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		var active []warden.Rule
		active, stale = warden.ArchiveStale(rf.Rules, opts.ArchiveAfterDays, now)
		if len(stale) > 0 {
			rf.Rules = active
			log.Printf("[smelter] archived %d stale rule(s) for %s (threshold=%dd)", len(stale), opts.AnvilName, opts.ArchiveAfterDays)
			if opts.EventLogger != nil {
				opts.EventLogger("smelter_flushed",
					fmt.Sprintf("Archived %d stale rule(s) for %s", len(stale), opts.AnvilName))
			}
		}
	}

	if opts.MaxRulesInFile > 0 {
		now := opts.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		active, evicted := warden.EvictOverCap(rf.Rules, opts.MaxRulesInFile, now)
		if len(evicted) > 0 {
			rf.Rules = active
			stale = append(stale, evicted...)
			log.Printf("[smelter] evicted %d rule(s) over the file ceiling for %s (max=%d, kept=%d)",
				len(evicted), opts.AnvilName, opts.MaxRulesInFile, len(active))
			if opts.EventLogger != nil {
				opts.EventLogger("smelter_flushed",
					fmt.Sprintf("Evicted %d rule(s) over the %d-rule ceiling for %s",
						len(evicted), opts.MaxRulesInFile, opts.AnvilName))
			}
		}
	}

	backfilled := pathsBackfill(ctx, opts.AnvilPath, opts.AnvilName, rf)
	if len(backfilled) > 0 {
		log.Printf("[smelter] paths backfilled on %d rule(s) for %s", len(backfilled), opts.AnvilName)
		if opts.EventLogger != nil {
			opts.EventLogger("smelter_flushed",
				fmt.Sprintf("Backfilled paths on %d rule(s) for %s", len(backfilled), opts.AnvilName))
		}
	}

	// Contradictions are reported, never resolved, so they are computed last
	// (over the rules as they will be written) and left out of HasChanges:
	// a run whose only finding is a contradiction has nothing to persist.
	//
	// Through the same reporter the scheduled flush uses, so the log line and
	// the feed event cannot come to differ between the two paths — and with
	// no announcer, since this is a one-shot command: every pair is news to
	// the operator who just typed `forge warden consolidate`, and suppressing
	// across invocations is what the daemon's per-anvil memory is for.
	contradictions := reportContradictions(opts.AnvilName, rf.Rules, nil, opts.EventLogger)

	passes := PassResults{
		Consolidated:   summary,
		Archived:       stale,
		Backfilled:     backfilled,
		Contradictions: contradictions,
	}

	result := ConsolidateResult{
		Passes:        passes,
		Pass1Archived: replaced,
		InitialCount:  initialCount,
		FinalActive:   len(rf.Rules),
		FirstError:    firstPassErr,
	}

	if !passes.HasChanges() {
		// Even with no changes, surface the archive size so the CLI can
		// report the file is already at steady state.
		if a, err := warden.LoadArchive(warden.ArchivePath(opts.AnvilPath)); err == nil && a != nil {
			result.ArchiveCount = len(a.Rules)
		}
		return result, nil
	}

	if err := persistRulesAndArchive(opts.AnvilPath, rf, replaced, summary, stale); err != nil {
		return result, fmt.Errorf("persisting warden rules: %w", err)
	}

	// Re-read the archive after writing so the reported size reflects what
	// landed on disk (deduplicated by ID).
	if a, err := warden.LoadArchive(warden.ArchivePath(opts.AnvilPath)); err == nil && a != nil {
		result.ArchiveCount = len(a.Rules)
	}
	return result, nil
}
