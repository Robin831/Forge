package queueactions

import "errors"

// ErrForgeMismatch is returned when the caller's forge_id does not match the
// forge that owns the worker/bead being acted on. This is the multi-forge
// safety check that prevents one Forge instance from clobbering another's
// state when they share a database or are addressed by a shared client (e.g.
// the web UI).
var ErrForgeMismatch = errors.New("forge_id does not match owning forge")

// ErrMissingBeadID indicates the caller did not supply a bead identifier.
var ErrMissingBeadID = errors.New("bead_id is required")

// ErrMissingAnvil indicates the caller did not supply an anvil name.
var ErrMissingAnvil = errors.New("anvil is required")

// ErrMissingReason indicates the caller did not supply a reason/note where
// the action requires one (currently: clarify).
var ErrMissingReason = errors.New("reason is required")
