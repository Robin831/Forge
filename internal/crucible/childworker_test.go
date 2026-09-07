package crucible

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/vcs"
)

// A child pipeline inserts a worker row of its own, stamped with the RUNNING
// daemon generation — so the generation cannot tell it from a leak and the
// daemon's live-worker registry is the only evidence that anything owns it.
// Letting pipeline.Run mint the id (Params.WorkerID empty) put that id where
// the daemon could never see it: the row went unregistered, unheartbeated, and
// was reaped as leaked three minutes in, marking a live child failed and handing
// its dispatch slot back while its Smith session was still running.
//
// So each child's id is minted here and registered BEFORE Run inserts anything.
func TestChildPipelineWorkerIsMintedAndRegistered(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var mu sync.Mutex
	held := map[string]bool{} // registered right now
	seen := map[string]bool{} // registered at any point
	released := map[string]int{}
	childWorkerIDs := map[string]string{}
	heldDuringRun := map[string]bool{}

	prCounter := 0
	p := Params{
		DB:                        db,
		Logger:                    logger,
		ParentBead:                poller.Bead{ID: "parent-1", Title: "Parent bead"},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		TrackWorker: func(id string) func() {
			mu.Lock()
			held[id] = true
			seen[id] = true
			mu.Unlock()
			return func() {
				mu.Lock()
				delete(held, id)
				released[id]++
				mu.Unlock()
			}
		},

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error { return nil },
		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
				{ID: "child-2", Title: "Second child", DependsOn: []string{"child-1", "parent-1"}},
			}, nil
		},
		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			mu.Lock()
			childWorkerIDs[pp.Bead.ID] = pp.WorkerID
			heldDuringRun[pp.Bead.ID] = held[pp.WorkerID]
			mu.Unlock()
			return &pipeline.Outcome{Success: true, Branch: "forge/" + pp.Bead.ID}
		},
		PRCreator: func(ctx context.Context, cp vcs.CreateParams) (*vcs.PR, error) {
			prCounter++
			return &vcs.PR{Number: prCounter, URL: fmt.Sprintf("https://example.test/pr/%d", prCounter)}, nil
		},
		PRMerger:    func(ctx context.Context, prNumber int, dir string) error { return nil },
		BeadClaimer: func(ctx context.Context, beadID, dir string) error { return nil },
		BeadCloser:  func(ctx context.Context, beadID, dir string) error { return nil },
	}

	if result := Run(context.Background(), p); result.Error != nil {
		t.Fatalf("crucible run failed: %v", result.Error)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, child := range []string{"child-1", "child-2"} {
		id := childWorkerIDs[child]
		if id == "" {
			t.Errorf("%s: pipeline.Params.WorkerID was empty — the pipeline would mint an "+
				"id the daemon never sees, so its worker row is inserted unowned", child)
			continue
		}
		if !seen[id] {
			t.Errorf("%s: worker id %q was never registered", child, id)
		}
		// Registered while the pipeline runs — the window in which the row
		// exists and the reaper can read it — not merely at some point.
		if !heldDuringRun[child] {
			t.Errorf("%s: worker id %q was not registered while the pipeline ran", child, id)
		}
		if released[id] != 1 {
			t.Errorf("%s: worker id %q released %d times, want exactly 1 (a hold "+
				"never released keeps a finished row un-reapable forever)", child, id, released[id])
		}
	}

	if childWorkerIDs["child-1"] == childWorkerIDs["child-2"] {
		t.Errorf("both children shared worker id %q; one row would be overwritten by the other",
			childWorkerIDs["child-1"])
	}
	if len(held) != 0 {
		t.Errorf("registry holds outlived the run: %v", held)
	}
}

// A nil TrackWorker is the pre-existing behaviour (tests, any caller with no
// registry) and must not be a nil dereference in the child dispatch path.
func TestChildPipelineRunsWithoutARegistry(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var gotWorkerID string
	prCounter := 0
	p := Params{
		DB:                        db,
		Logger:                    logger,
		ParentBead:                poller.Bead{ID: "parent-1", Title: "Parent bead"},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error { return nil },
		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}}}, nil
		},
		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			gotWorkerID = pp.WorkerID
			return &pipeline.Outcome{Success: true, Branch: "forge/" + pp.Bead.ID}
		},
		PRCreator: func(ctx context.Context, cp vcs.CreateParams) (*vcs.PR, error) {
			prCounter++
			return &vcs.PR{Number: prCounter, URL: "https://example.test/pr/1"}, nil
		},
		PRMerger:    func(ctx context.Context, prNumber int, dir string) error { return nil },
		BeadClaimer: func(ctx context.Context, beadID, dir string) error { return nil },
		BeadCloser:  func(ctx context.Context, beadID, dir string) error { return nil },
	}

	if result := Run(context.Background(), p); result.Error != nil {
		t.Fatalf("crucible run failed with no TrackWorker: %v", result.Error)
	}
	if gotWorkerID == "" {
		t.Error("child pipeline got no worker id even without a registry; the id is " +
			"minted here either way so the two paths insert the same row")
	}
}
