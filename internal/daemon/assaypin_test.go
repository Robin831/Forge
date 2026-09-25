package daemon

// Tests for pinned Assay reruns (`forge assay rerun <pr> --sha <commit>`): the
// commit validation, the base..sha range handed to the engine, and the handler
// that dispatches it.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/state"
)

func gitOutInTestRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = cleanGitTestEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "git %v", args)
	return strings.TrimSpace(string(out))
}

// pinTestRepo is a PR history: main A, PR commits B and C on top of A, main
// then moving on to D, and an unrelated branch commit E. The PR branch is
// deleted from origin, as after a merge, so its head survives only as
// refs/pull/7/head.
type pinTestRepo struct {
	anvil      string
	a, b, c, e string
	shortB     string
}

func newPinTestRepo(t *testing.T) pinTestRepo {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	anvil := filepath.Join(base, "anvil")

	gitInTestRepo(t, base, "init", "--bare", "-b", "main", remote)
	gitInTestRepo(t, base, "init", "-b", "main", seed)
	commit := func(file, content, msg string) string {
		require.NoError(t, os.WriteFile(filepath.Join(seed, file), []byte(content), 0o644))
		gitInTestRepo(t, seed, "add", file)
		gitInTestRepo(t, seed, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", msg)
		return gitOutInTestRepo(t, seed, "rev-parse", "HEAD")
	}
	gitInTestRepo(t, seed, "remote", "add", "origin", remote)

	r := pinTestRepo{anvil: anvil}
	r.a = commit("a.go", "package a\n", "A")
	gitInTestRepo(t, seed, "push", "-q", "origin", "main")

	gitInTestRepo(t, seed, "checkout", "-q", "-b", "pr")
	r.b = commit("b.go", "package b // defect\n", "B")
	r.c = commit("b.go", "package b // fixed\n", "C")
	gitInTestRepo(t, seed, "push", "-q", "origin", "pr:refs/pull/7/head")

	gitInTestRepo(t, seed, "checkout", "-q", "main")
	commit("d.go", "package d\n", "D")
	gitInTestRepo(t, seed, "push", "-q", "origin", "main")

	gitInTestRepo(t, seed, "checkout", "-q", "-b", "other", r.a)
	r.e = commit("e.go", "package e\n", "E")
	gitInTestRepo(t, seed, "push", "-q", "origin", "other")

	gitInTestRepo(t, base, "clone", "-q", remote, anvil)
	r.shortB = r.b[:7]
	return r
}

func TestResolveAssayPin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := newPinTestRepo(t)
	ctx := context.Background()

	t.Run("a PR commit pins to itself with the fork point as base", func(t *testing.T) {
		pin, err := resolveAssayPin(ctx, r.anvil, 7, "main", r.c, r.shortB)
		require.NoError(t, err)
		require.Equal(t, r.b, pin.SHA, "an abbreviated sha resolves to the full id")
		require.Equal(t, r.a, pin.Base, "base is merge-base(sha, origin/main), not the moved-on main tip")
	})

	t.Run("the PR head itself is a valid pin", func(t *testing.T) {
		pin, err := resolveAssayPin(ctx, r.anvil, 7, "", r.c, r.c)
		require.NoError(t, err)
		require.Equal(t, r.c, pin.SHA)
		require.Equal(t, r.a, pin.Base, "an empty base branch falls back to origin/main")
	})

	t.Run("an unknown sha is refused", func(t *testing.T) {
		_, err := resolveAssayPin(ctx, r.anvil, 7, "main", r.c, "deadbeefdeadbeef")
		require.ErrorContains(t, err, "not found in the repository")
	})

	t.Run("a commit outside the PR is refused", func(t *testing.T) {
		_, err := resolveAssayPin(ctx, r.anvil, 7, "main", r.c, r.e)
		require.ErrorContains(t, err, "is not in PR #7's history")
	})

	t.Run("a base-branch commit is refused as an empty range", func(t *testing.T) {
		_, err := resolveAssayPin(ctx, r.anvil, 7, "main", r.c, r.a)
		require.ErrorContains(t, err, "already on origin/main")
	})

	t.Run("a value that is not a commit id never reaches git", func(t *testing.T) {
		_, err := resolveAssayPin(ctx, r.anvil, 7, "main", r.c, "--upload-pack=x")
		require.ErrorContains(t, err, "invalid sha")
	})

	t.Run("an unknown PR head cannot anchor the check", func(t *testing.T) {
		_, err := resolveAssayPin(ctx, r.anvil, 7, "main", "", r.b)
		require.ErrorContains(t, err, "head commit is unknown")
	})
}

func TestFetchAssayPinnedDiffChecksOutAndDiffsTheRange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := newPinTestRepo(t)
	gitInTestRepo(t, r.anvil, "fetch", "-q", "origin", "refs/pull/7/head")

	d := &Daemon{}
	out, err := d.fetchAssayPinnedDiff(context.Background(), r.anvil, assayPin{SHA: r.b, Base: r.a})
	require.NoError(t, err)
	require.Contains(t, string(out), "+package b // defect", "the diff is base..sha")
	require.NotContains(t, string(out), "fixed", "nothing after sha is reviewed")
	require.NotContains(t, string(out), "d.go", "main's later commits are not in the range")
	require.Equal(t, r.b, gitOutInTestRepo(t, r.anvil, "rev-parse", "HEAD"), "the passes read files as of sha")
}

const pinnedDiff = "diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -0,0 +1 @@\n+package b // defect\n"

func TestRunAssayReviewPinnedReviewsBaseToSHA(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	// A live-posting anvil: the pin must still force shadow mode.
	d.cfg.Store(&config.Config{Assay: config.AssayConfig{
		Enabled: assayTestBool(true), ShadowMode: assayTestBool(false),
	}})
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) {
		t.Fatal("a pinned run must not fetch the PR head diff")
		return nil, nil
	}
	d.assayDeltaFetch = func(context.Context, string, string, string) ([]byte, error) {
		t.Fatal("a pinned run is never incremental")
		return nil, nil
	}
	pin := assayPin{SHA: "46e0f72f946a0000000000000000000000000000", Base: "a1b2c3d4e5f60000000000000000000000000000"}
	var gotPin assayPin
	d.assayPinnedDiff = func(_ context.Context, _ string, p assayPin) ([]byte, error) {
		gotPin = p
		return []byte(pinnedDiff), nil
	}
	var captured assay.ReviewRequest
	var gotDB *state.DB = db
	var gotShadow bool
	d.assayReview = func(_ context.Context, req assay.ReviewRequest, rdb *state.DB, cfg assay.Config) (*assay.ReviewResult, error) {
		captured, gotDB, gotShadow = req, rdb, cfg.ShadowMode
		return &assay.ReviewResult{Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
			Findings: []assay.Finding{{File: "b.go", Anchor: "b.go:1", Severity: "Major", Title: "unbounded Contains"}}}, nil
	}
	seedReviewedRun(t, db, "headsha")

	run, err := d.runAssayReview(context.Background(), "forge", t.TempDir(), "Forge-abc1", 347, "headsha", t.TempDir(), "", &pin)
	require.NoError(t, err)

	require.Equal(t, pin, gotPin, "the diff range is base..sha")
	require.Equal(t, pinnedDiff, captured.Diff)
	require.Equal(t, pin.SHA, captured.HeadSHA)
	require.False(t, captured.Incremental)
	require.Nil(t, gotDB, "a pinned run neither reads nor writes the PR's findings")
	require.True(t, gotShadow, "a pinned run is shadow mode whatever the anvil says")

	require.True(t, run.Pinned)
	require.True(t, run.ShadowMode)
	require.Equal(t, pin.SHA, run.HeadSHA, "the record names the commit reviewed")
	require.Zero(t, run.PostedCount)

	last, err := db.LastReviewedSHA("forge", 347)
	require.NoError(t, err)
	require.Equal(t, "headsha", last, "a pinned run does not move the head the gate sees as reviewed")
	n, err := db.CountAssayRuns("forge", 347)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a pinned run does not consume the per-PR run cap")

	events, err := db.RecentEvents(10)
	require.NoError(t, err)
	var msgs []string
	for _, ev := range events {
		msgs = append(msgs, ev.Message)
	}
	// The duration is wall clock, so only the parts around it are pinned.
	feed := strings.Join(msgs, "\n")
	require.Contains(t, feed, "Assay PR #347 at 46e0f72f946a: complete — 5/5 passes, 1 findings ($0.00, ")
	require.Contains(t, feed, "s) (pinned shadow run — findings in daemon log only)")
}

func TestRunAssayReviewUnpinnedKeepsHeadDiffAndDB(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayPinnedDiff = func(context.Context, string, assayPin) ([]byte, error) {
		t.Fatal("a head review must not take the pinned diff path")
		return nil, nil
	}
	var gotDB *state.DB
	var captured assay.ReviewRequest
	d.assayReview = func(_ context.Context, req assay.ReviewRequest, rdb *state.DB, _ assay.Config) (*assay.ReviewResult, error) {
		captured, gotDB = req, rdb
		return &assay.ReviewResult{Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5}, nil
	}

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.Same(t, db, gotDB)
	require.Equal(t, "deadbeef", captured.HeadSHA)
	require.Equal(t, "diff --git a/a.go b/a.go\n", captured.Diff, "the head review reads the PR diff")
	require.False(t, run.Pinned)
}

func TestHandleIPC_AssayRerunPinned(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	dispatched := make(chan lifecycle.ActionRequest, 1)
	d, _ := newAssayRunDaemon(t)
	d.db = db
	d.runCtx = context.Background()
	d.lifecycleDispatch = func(_ context.Context, req lifecycle.ActionRequest) { dispatched <- req }
	d.cfg.Store(&config.Config{Anvils: map[string]config.AnvilConfig{"munin": {Path: t.TempDir()}}})
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 5391, Anvil: "munin", BeadID: "BD-PIN", Branch: "forge/BD-PIN",
		BaseBranch: "main", Status: state.PROpen, CreatedAt: time.Now(),
	}))

	send := func(sha string) (string, map[string]string) {
		payload, _ := json.Marshal(ipc.AssayRerunPayload{Anvil: "munin", PRNumber: 5391, SHA: sha})
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: payload})
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		return resp.Type, msg
	}

	t.Run("a sha that fails validation is refused before dispatch", func(t *testing.T) {
		d.assayPinResolve = func(*state.PR, string, string) (*assayPin, error) {
			return nil, errors.New("commit 46e0f72 is not in PR #5391's history (head 0b1f3c49616f)")
		}
		typ, msg := send("46e0f72")
		require.Equal(t, "error", typ)
		require.Contains(t, msg["message"], "not in PR #5391's history")
		select {
		case req := <-dispatched:
			t.Fatalf("nothing should be dispatched, got %+v", req)
		default:
		}
	})

	t.Run("a valid sha dispatches a pinned manual review", func(t *testing.T) {
		pin := &assayPin{SHA: "46e0f72f946a0000000000000000000000000000", Base: "a1b2c3d4e5f60000000000000000000000000000"}
		var gotSHA string
		d.assayPinResolve = func(pr *state.PR, _ string, sha string) (*assayPin, error) {
			require.Equal(t, 5391, pr.Number)
			gotSHA = sha
			return pin, nil
		}
		typ, msg := send("46e0f72")
		require.Equal(t, "ok", typ)
		require.Equal(t, "46e0f72", gotSHA)
		require.Contains(t, msg["message"], "Assay re-review started for PR #5391 at 46e0f72f946a")

		select {
		case req := <-dispatched:
			require.Equal(t, lifecycle.ActionAssayReview, req.Action)
			require.True(t, req.IsManual)
			require.Equal(t, pin.SHA, req.PinnedSHA)
			require.Equal(t, pin.Base, req.PinnedBase)
			require.Equal(t, pin.SHA, req.HeadSHA)
			require.Equal(t, "forge/BD-PIN", req.Branch)
		case <-time.After(5 * time.Second):
			t.Fatal("pinned review was not dispatched")
		}
	})
}
