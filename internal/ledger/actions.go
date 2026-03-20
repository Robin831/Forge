package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Action result messages returned by bead CRUD commands.

// BeadCreatedMsg indicates a new bead was successfully created.
type BeadCreatedMsg struct{ ID string }

// BeadUpdatedMsg indicates a bead was successfully updated.
type BeadUpdatedMsg struct{ ID string }

// BeadClosedMsg indicates a bead was successfully closed.
type BeadClosedMsg struct{ ID string }

// BeadReopenedMsg indicates a bead was successfully reopened.
type BeadReopenedMsg struct{ ID string }

// ActionErrorMsg indicates a bead action failed.
type ActionErrorMsg struct{ Err error }

// NewBeadCmd creates a new bead via bd create.
func NewBeadCmd(anvilPath, title, description, issueType string, priority int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		args := []string{
			"create",
			"--title", title,
			"--description", description,
			"--type", issueType,
			"--priority", fmt.Sprintf("%d", priority),
			"--json",
		}
		out, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("create bead: %w", err)}
		}
		// Try to extract the ID from the JSON output — bd create --json
		// returns the created bead. We just report success with best-effort ID.
		id := extractIDFromJSON(out)
		return BeadCreatedMsg{ID: id}
	}
}

// EditBeadCmd updates a bead's title and description.
func EditBeadCmd(anvilPath, beadID, title, description string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		args := []string{"update", beadID, "--title", title, "--description", description}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("update %s: %w", beadID, err)}
		}
		return BeadUpdatedMsg{ID: beadID}
	}
}

// CloseBeadCmd closes a bead with an optional reason.
func CloseBeadCmd(anvilPath, beadID, reason string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		args := []string{"close", beadID}
		if reason != "" {
			args = append(args, "--reason", reason)
		}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("close %s: %w", beadID, err)}
		}
		return BeadClosedMsg{ID: beadID}
	}
}

// ReopenBeadCmd reopens a closed bead.
func ReopenBeadCmd(anvilPath, beadID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		args := []string{"update", beadID, "--status=open"}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("reopen %s: %w", beadID, err)}
		}
		return BeadReopenedMsg{ID: beadID}
	}
}

// extractIDFromJSON does a best-effort extraction of the "id" field from
// bd create --json output. Returns empty string on failure.
func extractIDFromJSON(data []byte) string {
	// Simple approach: unmarshal into a Bead struct.
	var b Bead
	if err := json.Unmarshal(data, &b); err != nil {
		return ""
	}
	return b.ID
}
