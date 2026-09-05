package executil

import "strings"

// The three readings of bd's output below are claims about BD's behaviour, not
// about any caller's — which is why they live here beside the single bd entry
// point rather than being copied into each scanner that talks to it. Four
// packages had a copy each before this, and the one that matters most is
// BdReportsNoSuchBead: a copy that stops agreeing with the others turns a
// timeout into "that bead is gone", which is how a pin gets dropped and a
// duplicate bead created.

// BdReportsNoSuchBead reports whether bd's output is its "no such bead" answer
// rather than a failure to reach the database.
//
// bd exits non-zero for BOTH, so the exit code decides nothing and the message
// is the only thing that separates them. The distinction is load-bearing
// wherever a recorded bead id is resolved: reading a timeout as an absent bead
// forgets the record and files the second bead the record exists to prevent.
func BdReportsNoSuchBead(out []byte) bool {
	var resp struct {
		Error string `json:"error"`
	}
	if DecodeJSON(out, &resp) != nil {
		return false
	}
	msg := strings.ToLower(resp.Error)
	return strings.Contains(msg, "no issue found") || strings.Contains(msg, "no issues found")
}

// BdCreatedBeadID reads the id out of a `bd create --json` response, or "" when
// it cannot be read. bd may emit trailing diagnostics after the JSON object
// (orphan-detection warnings, for one), which is why this goes through
// DecodeJSON rather than json.Unmarshal.
func BdCreatedBeadID(out []byte) string {
	var created struct {
		ID string `json:"id"`
	}
	if DecodeJSON(out, &created) != nil {
		return ""
	}
	return created.ID
}

// DecodeOneBead reads a single bead from bd output, which is an array on some
// bd versions and a bare object on others.
//
// The bead type is the caller's, because what a caller needs off a bead differs
// — depcheck reads labels and timestamps, the scanners that only need to know a
// bead still exists read neither — and a shared struct would be the union of
// every caller's needs. What is shared is the SHAPE of the answer, so id is
// passed in: a decode that "succeeded" into a zero-valued struct is not a bead,
// and without an id there is nothing to tell the two apart.
func DecodeOneBead[T any](out []byte, id func(T) string) *T {
	if len(out) == 0 || id == nil {
		return nil
	}
	var beads []T
	if err := DecodeJSON(out, &beads); err == nil && len(beads) > 0 && id(beads[0]) != "" {
		return &beads[0]
	}
	var single T
	if err := DecodeJSON(out, &single); err == nil && id(single) != "" {
		return &single
	}
	return nil
}
