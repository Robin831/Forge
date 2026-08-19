package daemon

import (
	"bytes"
	"context"

	"github.com/Robin831/Forge/internal/executil"
)

// decodeBdShow decodes one `bd show --json` payload into T.
//
// bd answers with either a bare object or a single-element array depending on
// the command and version, so every caller needs both forms; this is the one
// place that fallback lives, rather than a hand-rolled copy per call site that
// can drift from the others. An array with no elements is a failure, not an
// empty T: it names no bead, and a caller reading a zero value out of it would
// see "status: not closed" or "no dependents" where bd said nothing at all.
//
// The reported error is always the object-form one — the array attempt is the
// fallback, so its parse error describes the shape the payload was not.
func decodeBdShow[T any](out []byte) (T, error) {
	var v T
	objErr := executil.DecodeJSON(out, &v)
	if objErr == nil {
		return v, nil
	}

	var vs []T
	if arrErr := executil.DecodeJSON(out, &vs); arrErr != nil || len(vs) == 0 {
		var zero T
		return zero, objErr
	}
	return vs[0], nil
}

// defaultBeadShower is the real implementation behind Daemon.beadShower: one
// `bd show --id=<id> --json --include-dependents` run against the anvil, giving
// back stdout, bd's diagnostics, and a classified error.
//
// It is a named function rather than a closure so the flag and the
// classification are reachable from a test with a fake bd on PATH — the wiring
// this exists to protect is exactly the wiring a stubbed beadShower replaces.
//
// The dependents array is requested for every consumer of this shower, not just
// maybeCloseDecomposedParent, so there is one bd invocation shape here rather
// than a flagged and an unflagged one to pick between. Without it bd omits the
// array (see executil.BdIncludeDependentsFlag) and maybeCloseDecomposedParent
// reads "no dependents" for every parent — the branch that auto-closes a bead
// its children are still blocked on. The two field lookups (status,
// external_ref) are unaffected by the extra data beyond its cost.
//
// The context is context.Background() so the bd show call succeeds even during
// graceful shutdown (d.runCtx may already be cancelled at that point); the
// subprocess is still bounded by settings.bd_timeout.
func defaultBeadShower(anvilPath, beadID string) (stdout []byte, stderr string, err error) {
	cmd, cancel := executil.BdShowDependents(context.Background(), beadID)
	defer cancel()
	cmd.Dir = anvilPath

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	out, runErr := cmd.Output()
	return out, stderrBuf.String(), executil.ClassifyBdShowError(runErr, stderrBuf.String())
}
