package daemon

import "github.com/Robin831/Forge/internal/executil"

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
