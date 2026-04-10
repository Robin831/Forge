package ledger

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/executil"
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
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
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
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
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
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		args := []string{"close", beadID, "--json"}
		if reason != "" {
			args = append(args, "--reason", reason)
		}
		_, err := bdCloseExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("close %s: %w", beadID, err)}
		}
		return BeadClosedMsg{ID: beadID}
	}
}

// ReopenBeadCmd reopens a closed bead.
func ReopenBeadCmd(anvilPath, beadID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		args := []string{"update", beadID, "--status=open"}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("reopen %s: %w", beadID, err)}
		}
		return BeadReopenedMsg{ID: beadID}
	}
}

// UpdateLabelCmd adds or removes a label from a bead.
func UpdateLabelCmd(anvilPath, beadID, label string, remove bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		flag := "--add-label"
		if remove {
			flag = "--remove-label"
		}
		args := []string{"update", beadID, flag, label}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("update label %s: %w", beadID, err)}
		}
		return BeadUpdatedMsg{ID: beadID}
	}
}

// UpdatePriorityCmd changes a bead's priority.
func UpdatePriorityCmd(anvilPath, beadID string, priority int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		args := []string{"update", beadID, "--priority", fmt.Sprintf("%d", priority)}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("update priority %s: %w", beadID, err)}
		}
		return BeadUpdatedMsg{ID: beadID}
	}
}

// AppendNotesCmd appends text to a bead's notes field.
func AppendNotesCmd(anvilPath, beadID, notes string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		args := []string{"update", beadID, "--append-notes", notes}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("append notes %s: %w", beadID, err)}
		}
		return BeadUpdatedMsg{ID: beadID}
	}
}

// UpdateNotesCmd replaces a bead's notes field.
func UpdateNotesCmd(anvilPath, beadID, notes string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		args := []string{"update", beadID, "--notes", notes}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("update notes %s: %w", beadID, err)}
		}
		return BeadUpdatedMsg{ID: beadID}
	}
}

// UpdateAssigneeCmd assigns or unassigns a bead.
func UpdateAssigneeCmd(anvilPath, beadID, assignee string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		args := []string{"update", beadID, "--assignee=" + assignee}
		_, err := bdExec(ctx, anvilPath, args...)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("assign %s: %w", beadID, err)}
		}
		return BeadUpdatedMsg{ID: beadID}
	}
}

// DepAddedMsg indicates a dependency was successfully added.
type DepAddedMsg struct {
	BeadID string
	DepID  string
}

// DepRemovedMsg indicates a dependency was successfully removed.
type DepRemovedMsg struct {
	BeadID string
	DepID  string
}

// AddDepCmd adds a dependency to a bead via bd dep add <beadID> <depID>.
// After this, beadID depends on depID (depID blocks beadID).
func AddDepCmd(anvilPath, beadID, depID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		_, err := bdExec(ctx, anvilPath, "dep", "add", beadID, depID)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("add dep %s→%s: %w", beadID, depID, err)}
		}
		return DepAddedMsg{BeadID: beadID, DepID: depID}
	}
}

// RemoveDepCmd removes a dependency via bd dep remove <beadID> <depID>.
// This removes the relationship where beadID depends on depID.
func RemoveDepCmd(anvilPath, beadID, depID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
		defer cancel()

		_, err := bdExec(ctx, anvilPath, "dep", "remove", beadID, depID)
		if err != nil {
			return ActionErrorMsg{Err: fmt.Errorf("remove dep %s→%s: %w", beadID, depID, err)}
		}
		return DepRemovedMsg{BeadID: beadID, DepID: depID}
	}
}

// extractIDFromJSON does a best-effort extraction of the "id" field from
// bd create --json output. The output may be a JSON array (e.g. [{"id":...}])
// or a plain object, and may contain trailing diagnostics (e.g. orphan
// detection warnings). Returns empty string on failure.
func extractIDFromJSON(data []byte) string {
	// Try as array first — bd --json typically wraps results in an array.
	var arr []Bead
	if err := executil.DecodeJSON(data, &arr); err == nil && len(arr) > 0 {
		return arr[0].ID
	}
	// Fall back to single object.
	var b Bead
	if err := executil.DecodeJSON(data, &b); err != nil {
		return ""
	}
	return b.ID
}
