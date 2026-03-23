package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// Bead represents an issue from bd with enrichment from Forge's state DB.
type Bead struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	Priority    int        `json:"priority"`
	IssueType   string     `json:"issue_type"`
	Assignee    string     `json:"assignee"`
	Labels      []string   `json:"labels"`
	Blocks      []string   `json:"blocks"`
	DependsOn   []string   `json:"depends_on"`
	ClosedAt    *time.Time `json:"closed_at"`
	UpdatedAt   *time.Time `json:"updated_at"`

	// Enriched fields (not from bd JSON)
	Anvil string `json:"-"`
	HasPR bool   `json:"-"`
}

// beadJSON is an internal mirror of Bead used only for JSON deserialization.
// It uses json.RawMessage for timestamp fields so we can sanitise out-of-range
// years before storing them as *time.Time.
type beadJSON struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Status      string          `json:"status"`
	Priority    int             `json:"priority"`
	IssueType   string          `json:"issue_type"`
	Assignee    string          `json:"assignee"`
	Labels      []string        `json:"labels"`
	Blocks      []string        `json:"blocks"`
	DependsOn   []string        `json:"depends_on"`
	ClosedAt    json.RawMessage `json:"closed_at"`
	UpdatedAt   json.RawMessage `json:"updated_at"`
}

// UnmarshalJSON implements json.Unmarshaler for Bead.
// It sanitises timestamp fields whose year is outside the JSON-safe range
// [0,9999], zeroing those fields instead of storing an unrepresentable value
// that would crash when the Bead is later re-marshalled to JSON.
func (b *Bead) UnmarshalJSON(data []byte) error {
	var raw beadJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.ID = raw.ID
	b.Title = raw.Title
	b.Description = raw.Description
	b.Status = raw.Status
	b.Priority = raw.Priority
	b.IssueType = raw.IssueType
	b.Assignee = raw.Assignee
	b.Labels = raw.Labels
	b.Blocks = raw.Blocks
	b.DependsOn = raw.DependsOn
	b.ClosedAt = parseTimeSafe(raw.ClosedAt)
	b.UpdatedAt = parseTimeSafe(raw.UpdatedAt)
	return nil
}

// parseTimeSafe parses a JSON timestamp value and returns nil when the value is
// null, empty, unparseable, or has a year outside the range [0,9999].
// time.Time.MarshalJSON rejects years outside that range, so we sanitise at
// unmarshal time to prevent a later re-marshal from panicking.
func parseTimeSafe(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	y := t.Year()
	if y < 0 || y > 9999 {
		return nil
	}
	return &t
}

// UpdateBeadsMsg carries the result of a FetchAllBeads operation.
type UpdateBeadsMsg struct {
	Beads []Bead
	Err   error
}

// bdExecFunc is the function type for executing bd CLI commands.
// It is used for dependency injection in tests.
type bdExecFunc func(ctx context.Context, anvilPath string, args ...string) ([]byte, error)

// bdExec runs a bd command in the given anvil directory, hiding the console window.
func bdExec(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", args...))
	cmd.Dir = anvilPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bd %v in %s: %w: %s", args, anvilPath, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// FetchAnvilBeads returns a tea.Cmd that fetches beads for a single named anvil.
// It fetches open/in_progress beads plus recently-closed beads (last 7 days,
// up to 50), then enriches the results with PR data from the state DB.
func FetchAnvilBeads(anvilName, anvilPath string, db *state.DB) tea.Cmd {
	return fetchAnvilBeadsWithExec(bdExec, anvilName, anvilPath, db)
}

// fetchAnvilBeadsWithExec is the testable implementation of FetchAnvilBeads.
func fetchAnvilBeadsWithExec(execFn bdExecFunc, anvilName, anvilPath string, db *state.DB) tea.Cmd {
	return func() tea.Msg {
		var allBeads []Bead
		var firstErr error

		// Each status gets its own 3-minute timeout so a slow first call
		// doesn't starve subsequent ones. On slow Dolt connections a single
		// bd list can take 2+ minutes.
		for _, status := range []string{"open", "in_progress"} {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				out, err := execFn(ctx, anvilPath, "list", "--status="+status, "--limit", "0", "--json")
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("fetching %s beads for %s: %w", status, anvilName, err)
					}
					return
				}
				var beads []Bead
				if err := json.Unmarshal(out, &beads); err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("parsing %s beads for %s: %w", status, anvilName, err)
					}
					return
				}
				for i := range beads {
					beads[i].Anvil = anvilName
				}
				allBeads = append(allBeads, beads...)
			}()
		}

		// Fetch recently-closed beads (supplementary; failure is non-fatal).
		// Errors here are logged but don't prevent showing open/in_progress beads.
		{
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			out, err := execFn(ctx, anvilPath, "list", "--status=closed", "--limit", "50", "--json")
			if err == nil {
				var beads []Bead
				if err := json.Unmarshal(out, &beads); err == nil {
					cutoff := time.Now().AddDate(0, 0, -7)
					for i := range beads {
						beads[i].Anvil = anvilName
						if (beads[i].ClosedAt != nil && beads[i].ClosedAt.After(cutoff)) ||
							(beads[i].UpdatedAt != nil && beads[i].UpdatedAt.After(cutoff)) {
							allBeads = append(allBeads, beads[i])
						}
					}
				}
			}
			// Silently ignore closed-beads errors — open/in_progress are the priority.
		}

		// Enrich with PR data from state DB.
		if db != nil {
			openPRs, err := db.OpenPRs()
			if err == nil {
				prBeads := make(map[string]bool)
				for _, pr := range openPRs {
					if pr.BeadID != "" {
						prBeads[pr.BeadID] = true
					}
				}
				for i := range allBeads {
					if prBeads[allBeads[i].ID] {
						allBeads[i].HasPR = true
					}
				}
			}
		}

		return UpdateBeadsMsg{Beads: allBeads, Err: firstErr}
	}
}

// FetchAllBeads returns a tea.Cmd that fetches beads from all anvils in parallel.
// It fetches open/in_progress beads plus up to 50 closed beads (best-effort),
// then filters the closed beads to those updated or closed within the last 7 days.
// Note: closed-bead ordering is not guaranteed, so the 7-day window is applied
// after fetching — beads closed within 7 days but outside the first 50 results
// may not appear.
func FetchAllBeads(anvils map[string]string, db *state.DB) tea.Cmd {
	return fetchAllBeadsWithExec(bdExec, anvils, db)
}

// fetchAllBeadsWithExec is the internal implementation of FetchAllBeads that
// accepts an injectable bdExecFunc for testability.
func fetchAllBeadsWithExec(execFn bdExecFunc, anvils map[string]string, db *state.DB) tea.Cmd {
	return func() tea.Msg {
		var mu sync.Mutex
		var wg sync.WaitGroup
		var allBeads []Bead
		var firstErr error

		for name, path := range anvils {
			// Fetch open + in_progress beads — use a shorter timeout since these
			// should be fast; they are critical for the Ledger view.
			wg.Add(1)
			go func(name, path string) {
				defer wg.Done()
				// bd does not support multiple --status flags; make separate calls per status.
				// A single shared timeout context covers both calls so the combined wait
				// stays comparable to the old single-call deadline.
				openCtx, openCancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer openCancel()
				for _, status := range []string{"open", "in_progress"} {
					func() {
						out, err := execFn(openCtx, path, "list", "--status="+status, "--limit", "0", "--json")
						if err != nil {
							mu.Lock()
							if firstErr == nil {
								firstErr = fmt.Errorf("listing %s beads for anvil %s at %s: %w", status, name, path, err)
							}
							mu.Unlock()
							return
						}
						var beads []Bead
						if err := json.Unmarshal(out, &beads); err != nil {
							mu.Lock()
							if firstErr == nil {
								firstErr = fmt.Errorf("parsing %s beads for %s: %w", status, name, err)
							}
							mu.Unlock()
							return
						}
						for i := range beads {
							beads[i].Anvil = name
						}
						mu.Lock()
						allBeads = append(allBeads, beads...)
						mu.Unlock()
					}()
				}
			}(name, path)

			// Fetch recently closed beads — use a longer timeout since remote Dolt
			// anvils can be slow; closed beads are supplementary so a longer wait
			// here does not block the primary open/in_progress data.
			wg.Add(1)
			go func(name, path string) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				out, err := execFn(ctx, path, "list", "--status=closed", "--limit", "50", "--json")
				if err != nil {
					// Non-critical: closed beads are supplementary, but record the first error so the UI can surface it.
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("fetching closed beads for %s: %w", name, err)
					}
					mu.Unlock()
					return
				}
				var beads []Bead
				if err := json.Unmarshal(out, &beads); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("parsing closed beads for %s: %w", name, err)
					}
					mu.Unlock()
					return
				}
				cutoff := time.Now().AddDate(0, 0, -7)
				var recent []Bead
				for i := range beads {
					beads[i].Anvil = name
					if beads[i].ClosedAt != nil && beads[i].ClosedAt.After(cutoff) {
						recent = append(recent, beads[i])
					} else if beads[i].UpdatedAt != nil && beads[i].UpdatedAt.After(cutoff) {
						recent = append(recent, beads[i])
					}
				}
				mu.Lock()
				allBeads = append(allBeads, recent...)
				mu.Unlock()
			}(name, path)
		}
		wg.Wait()

		// Enrich with PR data from state DB
		if db != nil {
			openPRs, err := db.OpenPRs()
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("fetching open PRs: %w", err)
				}
				mu.Unlock()
			} else {
				prBeads := make(map[string]bool)
				for _, pr := range openPRs {
					if pr.BeadID != "" {
						prBeads[pr.BeadID] = true
					}
				}
				for i := range allBeads {
					if prBeads[allBeads[i].ID] {
						allBeads[i].HasPR = true
					}
				}
			}
		}

		return UpdateBeadsMsg{Beads: allBeads, Err: firstErr}
	}
}
