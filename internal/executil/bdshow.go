package executil

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BdIncludeDependentsFlag is the bd flag that makes `bd show --json` emit the
// full `dependents` array.
//
// It is a constant rather than a literal spelled at each call site because the
// array is opt-in and its absence is silent: bd (verified against 1.1.2) still
// reports `dependent_count` without the flag but omits `dependents` entirely,
// so a caller that forgets it decodes an empty slice and reads it as "this bead
// has no children" — the answer that closes an epic, auto-closes a decomposed
// parent, and keeps a Crucible from ever finding work.
const BdIncludeDependentsFlag = "--include-dependents"

// ErrIncludeDependentsUnsupported reports a bd too old to know
// BdIncludeDependentsFlag. It exists so that failure is loud: without the flag
// a bd show answers with no dependents at all, so silently retrying unflagged
// would hand every caller the same empty array the flag was added to fix.
// `forge doctor` checks for the flag up front for the same reason.
var ErrIncludeDependentsUnsupported = errors.New(
	"bd does not support " + BdIncludeDependentsFlag + " (requires bd 1.1.2 or newer); " +
		"upgrade bd — without it `bd show --json` omits the dependents array and Forge cannot see a bead's children")

// BdShowIDFlag is the bd flag that names a bead id explicitly instead of
// passing it positionally. bd documents it for exactly this ("use for IDs that
// look like flags"), and Forge uses it for every id it hands to `bd show`: bead
// ids come out of a dolt database that syncs through the git remote, so they
// are values Forge did not write, and one shaped like `-f` or
// `--include-dependents=x` would otherwise be consumed by bd's cobra parser as
// a flag — changing what the command means, or producing an "unknown flag"
// rejection ClassifyBdShowError would then read as an old bd.
const BdShowIDFlag = "--id"

// BdShowIDArgs renders bead ids as `--id=<id>` arguments. An empty id is
// dropped rather than sent as a valueless flag; every other value is passed
// through verbatim, since the point of the flag form is that no id needs
// screening to be safe.
func BdShowIDArgs(ids ...string) []string {
	args := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		args = append(args, BdShowIDFlag+"="+id)
	}
	return args
}

// BdShowArgs builds the argument vector for `bd show --id=<id>... --json`.
func BdShowArgs(ids ...string) []string {
	args := make([]string, 0, len(ids)+2)
	args = append(args, "show")
	args = append(args, BdShowIDArgs(ids...)...)
	args = append(args, "--json")
	return args
}

// BdShowDependentsArgs builds the argument vector for
// `bd show --id=<id>... --json --include-dependents`.
//
// Cost note: bd documents the flag as "may be slow on hub beads" because it
// streams each dependent's full record rather than a count. Forge pays that per
// bead it looks up, and poller.ResolveBlocks fans those lookups out concurrently
// across every ready bead whose Blocks are not already known. That is accepted
// rather than optimised: the alternative (an unflagged show to read
// dependent_count, then a flagged one only when it is non-zero) doubles the
// round trips for exactly the beads that matter — the parents — to save nothing
// on beads whose dependent list is empty and therefore cheap to stream.
func BdShowDependentsArgs(ids ...string) []string {
	return append(BdShowArgs(ids...), BdIncludeDependentsFlag)
}

// BdShowDependents builds a bounded
// `bd show --id=<id>... --json --include-dependents` command. It is the single
// entry point for every Forge call site that reads the dependents array, so the
// flag cannot be present on one path and missing on another. The caller owns
// the returned CancelFunc.
func BdShowDependents(parent context.Context, ids ...string) (*BdCmd, context.CancelFunc) {
	return BdCommand(parent, BdShowDependentsArgs(ids...)...)
}

// IsUnsupportedIncludeDependentsOutput reports whether bd's diagnostic output
// names BdIncludeDependentsFlag as an unknown flag. bd is a cobra program, so
// the rejection reads `Error: unknown flag: --include-dependents` on stderr with
// a non-zero exit; the stdlib flag package's wording is accepted too so the
// detection does not hinge on bd's argument parser staying cobra.
func IsUnsupportedIncludeDependentsOutput(output string) bool {
	lower := strings.ToLower(output)
	if !strings.Contains(lower, strings.TrimPrefix(BdIncludeDependentsFlag, "--")) {
		return false
	}
	return strings.Contains(lower, "unknown flag") ||
		strings.Contains(lower, "unknown shorthand flag") ||
		strings.Contains(lower, "flag provided but not defined")
}

// ClassifyBdShowError wraps a failed BdShowDependents run as
// ErrIncludeDependentsUnsupported when bd's diagnostics say it did not
// recognise the flag, and returns the original error otherwise. Callers pass
// whatever bd wrote — stderr for an Output() run, the combined buffer for a
// CombinedOutput() one.
func ClassifyBdShowError(err error, output string) error {
	if err == nil {
		return nil
	}
	if IsUnsupportedIncludeDependentsOutput(output) {
		return fmt.Errorf("%w: %v", ErrIncludeDependentsUnsupported, err)
	}
	return err
}
