package crucible

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/vcs"
)

func TestExcludeIndependent(t *testing.T) {
	children := []poller.Bead{
		{ID: "child-1"},
		{ID: "child-2", Labels: []string{"independent"}},
		{ID: "child-3", Labels: []string{"forgeReady"}},
		{ID: "child-4", Labels: []string{" Independent "}},
	}

	kept, excluded := excludeIndependent(children)

	if len(kept) != 2 || kept[0].ID != "child-1" || kept[1].ID != "child-3" {
		t.Errorf("kept = %v, want child-1 and child-3", beadIDs(kept))
	}
	if len(excluded) != 2 || excluded[0] != "child-2" || excluded[1] != "child-4" {
		t.Errorf("excluded = %v, want [child-2 child-4]", excluded)
	}
}

func TestExcludeIndependent_NothingExcluded(t *testing.T) {
	kept, excluded := excludeIndependent([]poller.Bead{{ID: "child-1"}})

	if len(kept) != 1 {
		t.Errorf("kept = %v, want child-1", beadIDs(kept))
	}
	if excluded != nil {
		t.Errorf("excluded = %v, want nil", excluded)
	}
}

// The dispatch half of the opt-out: a child carrying "independent" is never run
// on the feature branch, and its siblings are orchestrated as usual.
func TestRun_IndependentChildIsNotDispatched(t *testing.T) {
	var dispatched []string
	var createdPRs []vcs.CreateParams

	p := independentTestParams(t, func(_ context.Context, _, _ string) ([]poller.Bead, error) {
		return []poller.Bead{
			{ID: "child-1", Title: "First child"},
			{ID: "child-2", Title: "Opted out", Labels: []string{"independent"}},
		}, nil
	})
	p.PipelineRunner = func(_ context.Context, pp pipeline.Params) *pipeline.Outcome {
		dispatched = append(dispatched, pp.Bead.ID)
		return &pipeline.Outcome{Success: true, Branch: fmt.Sprintf("forge/%s", pp.Bead.ID)}
	}
	p.PRCreator = func(_ context.Context, params vcs.CreateParams) (*vcs.PR, error) {
		createdPRs = append(createdPRs, params)
		return &vcs.PR{Number: len(createdPRs)}, nil
	}

	result := Run(context.Background(), p)

	if len(dispatched) != 1 || dispatched[0] != "child-1" {
		t.Errorf("dispatched = %v, want only child-1", dispatched)
	}
	for _, pr := range createdPRs {
		if pr.BeadID == "child-2" {
			t.Error("the independent child must not get a PR onto the feature branch")
		}
	}
	if result.ChildrenTotal != 1 {
		t.Errorf("ChildrenTotal = %d, want 1 — the opted-out child is not part of the epic", result.ChildrenTotal)
	}
}

// The completeness half, and the decision this bead had to make: an
// "independent" child is excluded from the completeness set entirely, so an
// open one never pauses the epic. Its work reaches main through its own PR and
// could not have reached the feature branch, so waiting for it would hold the
// final PR for something the final PR cannot contain.
//
// The child here also carries an unresolved external dependency — exactly what
// makes an ordinary child a *skipped* one, which does pause the run — so this
// pins the opt-out taking precedence over the skip.
func TestRun_OpenIndependentChildDoesNotBlockTheFinalPR(t *testing.T) {
	var createdPRs []vcs.CreateParams
	var closedBeads []string

	p := independentTestParams(t, func(_ context.Context, _, _ string) ([]poller.Bead, error) {
		return []poller.Bead{
			{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
			{ID: "child-2", Title: "Opted out", Labels: []string{"independent"},
				DependsOn: []string{"parent-1", "external-dep"}},
		}, nil
	})
	p.PipelineRunner = func(_ context.Context, pp pipeline.Params) *pipeline.Outcome {
		return &pipeline.Outcome{Success: true, Branch: fmt.Sprintf("forge/%s", pp.Bead.ID)}
	}
	p.PRCreator = func(_ context.Context, params vcs.CreateParams) (*vcs.PR, error) {
		createdPRs = append(createdPRs, params)
		return &vcs.PR{Number: len(createdPRs)}, nil
	}
	p.BeadCloser = func(_ context.Context, beadID, _ string) error {
		closedBeads = append(closedBeads, beadID)
		return nil
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("the epic must not pause on an open independent child: %v", result.Error)
	}
	if result.ChildrenSkipped != 0 {
		t.Errorf("ChildrenSkipped = %d, want 0 — an opted-out child is excluded, not skipped", result.ChildrenSkipped)
	}
	if result.ChildrenDone != 1 || result.ChildrenTotal != 1 {
		t.Errorf("ChildrenDone/Total = %d/%d, want 1/1", result.ChildrenDone, result.ChildrenTotal)
	}
	var finalPR bool
	for _, pr := range createdPRs {
		if pr.BeadID == "parent-1" {
			finalPR = true
		}
	}
	if !finalPR {
		t.Error("the epic is complete, so the final PR must be created")
	}
	for _, b := range closedBeads {
		if b == "child-2" {
			t.Error("the Crucible must not close a bead it never ran")
		}
	}
}

// A parent whose every child opted out has nothing to orchestrate — the same
// state as a parent whose children are all closed.
func TestRun_AllChildrenIndependent_NothingToOrchestrate(t *testing.T) {
	var dispatched []string

	p := independentTestParams(t, func(_ context.Context, _, _ string) ([]poller.Bead, error) {
		return []poller.Bead{
			{ID: "child-1", Labels: []string{"independent"}},
			{ID: "child-2", Labels: []string{"independent"}},
		}, nil
	})
	p.PipelineRunner = func(_ context.Context, pp pipeline.Params) *pipeline.Outcome {
		dispatched = append(dispatched, pp.Bead.ID)
		return &pipeline.Outcome{Success: true, Branch: "forge/" + pp.Bead.ID}
	}

	result := Run(context.Background(), p)

	if !result.Success || result.ChildrenTotal != 0 {
		t.Errorf("result = %+v, want a no-op success", result)
	}
	if len(dispatched) != 0 {
		t.Errorf("dispatched = %v, want nothing", dispatched)
	}
}

// A bead carrying the opt-out is never a Crucible parent, whatever else it
// carries: an opt-in and an opt-out on one bead resolve toward independent.
func TestIsCrucibleCandidate_IndependentIsNeverAParent(t *testing.T) {
	bead := poller.Bead{
		ID:     "parent-1",
		Labels: []string{"crucible", "independent"},
		Blocks: []string{"child-1"},
	}

	if IsCrucibleCandidate(bead) {
		t.Error("IsCrucibleCandidate = true, want false for a bead labeled independent")
	}
}

// independentTestParams builds the injected-everything Params the Run tests in
// this file share: no git, no bd, no provider — only the seams the case under
// test overrides afterwards.
func independentTestParams(t *testing.T, fetch func(context.Context, string, string) ([]poller.Bead, error)) Params {
	t.Helper()
	return Params{
		DB:     testDB(t),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		ParentBead: poller.Bead{
			ID:     "parent-1",
			Title:  "Parent bead",
			Labels: []string{"crucible"},
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(_ context.Context, _, _ string) error { return nil },
		ChildFetcher:      fetch,
		BeadClaimer:       func(_ context.Context, _, _ string) error { return nil },
		BeadCloser:        func(_ context.Context, _, _ string) error { return nil },
		PRMerger:          func(_ context.Context, _ int, _ string) error { return nil },
		PRCreator: func(_ context.Context, _ vcs.CreateParams) (*vcs.PR, error) {
			return &vcs.PR{Number: 1}, nil
		},
	}
}

func beadIDs(beads []poller.Bead) []string {
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		out = append(out, b.ID)
	}
	return out
}

// The filter at the source: FetchChildren is what a live Crucible calls, and it
// must not even fetch an opted-out child's subtree — carving a bead out carves
// out the work hanging off it, which reaches main through that bead's own PR.
func TestFetchChildren_SkipsIndependentDescendantsAndTheirSubtree(t *testing.T) {
	withFakeBd(t, `case "$2" in
parent-1) echo '[{"id":"parent-1","status":"open","dependents":[{"id":"child-1","dependency_type":"blocks"},{"id":"child-2","dependency_type":"blocks"}]}]';;
child-1) echo '[{"id":"child-1","status":"open","labels":["independent"],"dependents":[{"id":"grandchild-1","dependency_type":"blocks"}]}]';;
child-2) echo '[{"id":"child-2","status":"open","labels":["forgeReady"]}]';;
grandchild-1) echo '[{"id":"grandchild-1","status":"open"}]';;
*) echo '[]';;
esac`)

	children, err := FetchChildren(context.Background(), "parent-1", t.TempDir())

	if err != nil {
		t.Fatalf("FetchChildren: %v", err)
	}
	got := beadIDs(children)
	if len(got) != 1 || got[0] != "child-2" {
		t.Errorf("children = %v, want only child-2 (child-1 opted out, taking grandchild-1 with it)", got)
	}
}

// withFakeBd puts a `bd` on PATH whose body is the given shell snippet, the same
// seam internal/poller's tests use to exercise bd-shaped code without a beads
// database.
func withFakeBd(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
