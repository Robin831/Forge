package state

import "time"

// ReconcilePollDivisor is how many main polls pass between PR reconcile runs.
// It lives here, beside the checker name, because two places need it and they
// must agree: the daemon's loop schedules on it, and the staleness threshold is
// derived from it. Kept apart, a change to the schedule would silently leave
// reconcile judged against a cadence it no longer runs at.
const ReconcilePollDivisor = 10

// DefaultStalenessMultiplier is how many of a checker's own intervals may pass
// with no completed cycle before it is reported.
//
// Three is deliberately loose. The cost of being late is that a broken checker
// goes unreported a while longer; the cost of being early is an entry for a
// checker that was merely slow, and a panel that cries wolf is one an operator
// stops reading — which is the failure this check exists to prevent, arriving
// by the other door.
const DefaultStalenessMultiplier = 3.0

// StalenessIntervals carries the configured cadence of each checker Forge can
// judge. It is a struct of durations rather than a config reference so the
// state package goes on knowing nothing about configuration, and so the one
// place mapping a checker onto its cadence is the package that owns the names.
type StalenessIntervals struct {
	Multiplier float64
	// A zero interval means the checker is DISABLED, not instantaneous, so it
	// is left out of the thresholds entirely.
	Depcheck   time.Duration
	Vulncheck  time.Duration
	Questgiver time.Duration
	// Poll is the main poll interval; PR reconcile's cadence derives from it
	// via ReconcilePollDivisor.
	Poll time.Duration
}

// StalenessThresholds maps each judgeable checker onto the age at which a
// missing cycle is reported.
//
// A checker with no entry is never judged. That is what stops a disabled
// scanner being reported as a broken one, and it is why the map is built from
// intervals rather than from the rows in the table: the rows say what HAS run,
// the configuration says what is supposed to.
func StalenessThresholds(in StalenessIntervals) map[string]time.Duration {
	mult := in.Multiplier
	if mult <= 0 {
		mult = DefaultStalenessMultiplier
	}
	scale := func(d time.Duration) time.Duration {
		return time.Duration(float64(d) * mult)
	}

	out := map[string]time.Duration{}
	if in.Depcheck > 0 {
		out[CheckerDepcheck] = scale(in.Depcheck)
	}
	if in.Vulncheck > 0 {
		out[CheckerVulncheck] = scale(in.Vulncheck)
	}
	if in.Questgiver > 0 {
		out[CheckerQuestgiver] = scale(in.Questgiver)
	}
	if in.Poll > 0 {
		out[CheckerPRReconcile] = scale(in.Poll * ReconcilePollDivisor)
	}
	return out
}
