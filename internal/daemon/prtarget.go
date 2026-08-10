package daemon

import (
	"errors"
	"fmt"

	"github.com/Robin831/Forge/internal/state"
)

// A PR reaches the daemon under one of two names: the `prs` table row id (what
// the web dashboard and Hearth hold, because that is what their list payloads
// carry) or the GitHub PR number scoped by an anvil (what an operator reads off
// a PR page, and the only form a CLI verb can reasonably ask for). Every IPC
// handler that needs the row itself resolves through resolvePRTarget, so the
// two forms have one lookup and one set of error messages between them.
var (
	// errNoPRTarget is returned when neither addressing form was supplied.
	errNoPRTarget = errors.New("a PR target is required: pr (state.db row id) or pr_number with an anvil")
	// errAmbiguousPRTarget is returned when both were. The two could name
	// different rows, and picking one silently is how a rerun lands on a PR
	// the caller was not looking at.
	errAmbiguousPRTarget = errors.New("specify either pr (state.db row id) or pr_number, not both")
	// errPRNumberNeedsAnvil is returned for a PR number with no anvil to scope
	// it: PR numbers are per-repository, so the pair is the identifier.
	errPRNumberNeedsAnvil = errors.New("pr_number requires an anvil")
)

// resolvePRTarget resolves a PR addressed by row id or by number+anvil to its
// `prs` row. Exactly one of prID and prNumber must be non-zero; anvil is
// required alongside prNumber and, when supplied alongside prID, is enforced as
// ownership so an id from one anvil cannot be re-reviewed under another's
// config. A row that does not exist is an error, never a nil row.
func resolvePRTarget(db *state.DB, prID, prNumber int, anvil string) (*state.PR, error) {
	switch {
	case prID > 0 && prNumber > 0:
		return nil, errAmbiguousPRTarget
	case prID <= 0 && prNumber <= 0:
		return nil, errNoPRTarget
	case prNumber > 0 && anvil == "":
		return nil, errPRNumberNeedsAnvil
	}

	if prNumber > 0 {
		pr, err := db.GetPRByNumber(anvil, prNumber)
		if err != nil {
			return nil, fmt.Errorf("failed to look up PR #%d on anvil %q: %w", prNumber, anvil, err)
		}
		if pr == nil {
			return nil, fmt.Errorf("PR #%d not found on anvil %q", prNumber, anvil)
		}
		return pr, nil
	}

	pr, err := db.GetPRByID(prID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up PR id %d: %w", prID, err)
	}
	if pr == nil {
		return nil, fmt.Errorf("PR id %d not found", prID)
	}
	if anvil != "" && pr.Anvil != anvil {
		return nil, fmt.Errorf("PR id %d belongs to anvil %q, not %q", prID, pr.Anvil, anvil)
	}
	return pr, nil
}

// resolvePRTargetPreferID is resolvePRTarget for callers that always send both
// forms — Hearth's PR list and the web PR rows carry the row id and the number
// together, so "both supplied" is their normal case rather than an ambiguity.
// The row id wins; the number is only consulted when there is no id (an
// externally-opened PR the dashboard knows by number alone).
func resolvePRTargetPreferID(db *state.DB, prID, prNumber int, anvil string) (*state.PR, error) {
	if prID > 0 {
		prNumber = 0
	}
	return resolvePRTarget(db, prID, prNumber, anvil)
}
