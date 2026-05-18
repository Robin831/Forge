package daemon

import (
	"errors"
	"fmt"

	"github.com/Robin831/Forge/internal/queueactions"
	"github.com/Robin831/Forge/internal/state"
)

// queueActionsErrorMessage formats a queueactions error for the IPC error
// payload. Required-field validation collapses to the legacy "bead_id and
// anvil are required" string so existing CLI/web callers keep matching;
// forge-mismatch is surfaced as-is so the sibling IPC handlers can show the
// caller what's wrong. Everything else is prefixed with the action name in
// the legacy "failed to X: <err>" shape.
func queueActionsErrorMessage(action string, err error) string {
	switch {
	case errors.Is(err, queueactions.ErrMissingBeadID),
		errors.Is(err, queueactions.ErrMissingAnvil):
		return "bead_id and anvil are required"
	case errors.Is(err, queueactions.ErrMissingReason):
		return "reason is required"
	case errors.Is(err, queueactions.ErrForgeMismatch):
		return err.Error()
	default:
		return fmt.Sprintf("failed to %s: %v", action, err)
	}
}

// queueActionsHandle adapts the daemon's state.DB and config snapshot to the
// queueactions.QueueHandle interface so the shared business logic can run
// from inside the daemon's IPC dispatcher without dragging the full Daemon
// type into the queueactions package.
type queueActionsHandle struct {
	db      *state.DB
	forgeID string
}

func (d *Daemon) queueActionsHandle() queueactions.QueueHandle {
	return &queueActionsHandle{
		db:      d.db,
		forgeID: d.cfg.Load().Settings.ResolvedForgeID(),
	}
}

func (h *queueActionsHandle) LocalForgeID() string { return h.forgeID }

func (h *queueActionsHandle) SetClarificationNeeded(beadID, anvil string, needed bool, reason string) error {
	return h.db.SetClarificationNeeded(beadID, anvil, needed, reason)
}

func (h *queueActionsHandle) ClearNeedsAttention(beadID, anvil string) error {
	return h.db.ClearNeedsAttention(beadID, anvil)
}

func (h *queueActionsHandle) GetRetry(beadID, anvil string) (*state.RetryRecord, error) {
	return h.db.GetRetry(beadID, anvil)
}

func (h *queueActionsHandle) ResetRetry(beadID, anvil string) error {
	return h.db.ResetRetry(beadID, anvil)
}

func (h *queueActionsHandle) ActiveWorkerByBeadAndAnvil(beadID, anvil string) (*state.Worker, error) {
	return h.db.ActiveWorkerByBeadAndAnvil(beadID, anvil)
}

func (h *queueActionsHandle) UpdateWorkerStatus(workerID string, status state.WorkerStatus) error {
	return h.db.UpdateWorkerStatus(workerID, status)
}

func (h *queueActionsHandle) LogEvent(typ state.EventType, message, beadID, anvil string) error {
	return h.db.LogEvent(typ, message, beadID, anvil)
}
