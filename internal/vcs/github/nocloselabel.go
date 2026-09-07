package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/vcs"
)

// noCloseLookupTimeout bounds the gh issue-label lookup so a slow GitHub API
// call can never stall PR creation for long.
const noCloseLookupTimeout = 30 * time.Second

// issueLabelFetcher fetches the label names on a GitHub issue. Replaced in
// tests to avoid spawning gh.
var issueLabelFetcher = fetchIssueLabels

// issueHasNoCloseLabel reports whether the ref's issue carries any of the
// configured no-close labels (vcs.NoCloseLabels, default `innmeldt`). Only a
// qualified ref can be looked up. Best-effort: a failed lookup returns false —
// the overwhelmingly common case is the bd-authored bead issue, which is
// closable, and refusing to close every bead issue whenever GitHub hiccups
// would be the worse failure mode. The failure is logged so a wrongly-closed
// no-close issue can be traced.
func issueHasNoCloseLabel(ctx context.Context, ref vcs.IssueRef) bool {
	if ref.IsZero() || !ref.Qualified() {
		return false
	}
	noClose := vcs.NoCloseLabels()
	if len(noClose) == 0 {
		return false
	}

	labels, err := issueLabelFetcher(ctx, ref)
	if err != nil {
		log.Printf("[vcs/github] no-close label lookup failed for %s (treating as closable): %v", ref.String(), err)
		return false
	}
	for _, have := range labels {
		for _, want := range noClose {
			if strings.EqualFold(have, want) {
				return true
			}
		}
	}
	return false
}

// fetchIssueLabels reads an issue's label names via the gh CLI.
func fetchIssueLabels(ctx context.Context, ref vcs.IssueRef) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, noCloseLookupTimeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "issue", "view", ref.Number,
		"-R", ref.Owner+"/"+ref.Repo, "--json", "labels"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, &lookupError{err: err, stderr: msg}
		}
		return nil, err
	}

	var resp struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.Labels))
	for _, l := range resp.Labels {
		names = append(names, l.Name)
	}
	return names, nil
}

// lookupError carries gh's stderr alongside the exec error.
type lookupError struct {
	err    error
	stderr string
}

func (e *lookupError) Error() string { return e.err.Error() + ": " + e.stderr }
func (e *lookupError) Unwrap() error { return e.err }
