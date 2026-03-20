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

// UpdateBeadsMsg carries the result of a FetchAllBeads operation.
type UpdateBeadsMsg struct {
	Beads []Bead
	Err   error
}

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

// FetchAllBeads returns a tea.Cmd that fetches beads from all anvils in parallel.
// It fetches open/in_progress beads plus recently closed beads (last 7 days),
// then enriches them with PR data from the state DB.
func FetchAllBeads(anvils map[string]string, db *state.DB) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		type result struct {
			beads []Bead
			err   error
		}

		var mu sync.Mutex
		var wg sync.WaitGroup
		var allBeads []Bead
		var firstErr error

		for name, path := range anvils {
			// Fetch open + in_progress beads
			wg.Add(1)
			go func(name, path string) {
				defer wg.Done()
				out, err := bdExec(ctx, path, "list", "--status=open", "--status=in_progress", "--json")
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				var beads []Bead
				if err := json.Unmarshal(out, &beads); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("parsing beads for %s: %w", name, err)
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
			}(name, path)

			// Fetch recently closed beads
			wg.Add(1)
			go func(name, path string) {
				defer wg.Done()
				out, err := bdExec(ctx, path, "list", "--status=closed", "--json")
				if err != nil {
					// Non-critical: closed beads are supplementary
					return
				}
				var beads []Bead
				if err := json.Unmarshal(out, &beads); err != nil {
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
