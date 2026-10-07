package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/vcs"
)

// queueRepo is a fake GitHub repository behind gh: a base branch that may or
// may not have a merge queue, and one PR whose state the test steers.
type queueRepo struct {
	queue       bool   // repository.mergeQueue(branch:) is non-null
	prState     string // OPEN / MERGED / CLOSED
	inQueue     bool
	autoMerge   bool
	mergeStdout string
	mergeStderr string
	mergeErr    error
	// onMerge mutates the PR when gh pr merge runs (what GitHub would do).
	onMerge func(r *queueRepo, args []string)
	gh      *fakeGH
}

func newQueueRepo(queue bool) *queueRepo {
	r := &queueRepo{queue: queue, prState: "OPEN"}
	r.gh = &fakeGH{handle: r.handle}
	return r
}

func (r *queueRepo) handle(args []string) (string, string, error) {
	q := argValue(args, "query=")
	switch {
	case args[0] == "pr" && args[1] == "merge":
		if r.onMerge != nil {
			r.onMerge(r, args)
		}
		return r.mergeStdout, r.mergeStderr, r.mergeErr
	case args[0] == "pr" && args[1] == "view" && slices.Contains(args, "baseRefName"):
		return `{"baseRefName":"main"}`, "", nil
	case strings.Contains(q, "mergeQueue(branch:"):
		if r.queue {
			return `{"data":{"repository":{"mergeQueue":{"id":"MQ_1"}}}}`, "", nil
		}
		return `{"data":{"repository":{"mergeQueue":null}}}`, "", nil
	case strings.Contains(q, "isInMergeQueue") && !isBatch(args):
		auto := "null"
		if r.autoMerge {
			auto = `{"enabledAt":"2026-10-07T10:00:00Z"}`
		}
		entry := "null"
		if r.inQueue {
			entry = `{"state":"QUEUED"}`
		}
		return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":%q,"isInMergeQueue":%v,"autoMergeRequest":%s,"mergeQueueEntry":%s}}}}`,
			r.prState, r.inQueue, auto, entry), "", nil
	}
	return "", "unexpected call: " + strings.Join(args, " "), errors.New("exit status 1")
}

func (r *queueRepo) mergeCalls() [][]string {
	var out [][]string
	for _, c := range r.gh.calls {
		if len(c) > 2 && c[0] == "gh" && c[1] == "pr" && c[2] == "merge" {
			out = append(out, c)
		}
	}
	return out
}

func (r *queueRepo) queueLookups() int {
	n := 0
	for _, c := range r.gh.calls {
		if strings.Contains(argValue(c, "query="), "mergeQueue(branch:") {
			n++
		}
	}
	return n
}

func assertNoAdmin(t *testing.T, r *queueRepo) {
	t.Helper()
	for _, c := range r.gh.calls {
		for _, a := range c {
			if strings.HasPrefix(a, "--admin") {
				t.Fatalf("gh was passed --admin, which bypasses the merge queue and required checks: %v", c)
			}
		}
	}
}

var mergeReq = vcs.MergeRequest{WorktreePath: "/repo", PRNumber: 7, Base: "main", Strategy: "squash"}

// A repo without a merge queue merges exactly as main always has: one
// `gh pr merge 7 --squash --delete-branch=false`, and nil means merged.
func TestMerge_NonQueueRepo_DirectMergeUnchanged(t *testing.T) {
	r := newQueueRepo(false)
	p := NewWithRunner(nil, r.gh.run, nil)

	outcome, err := p.MergePRWithOutcome(context.Background(), mergeReq)
	if err != nil || outcome != vcs.MergeOutcomeMerged {
		t.Fatalf("want merged/nil, got %v/%v", outcome, err)
	}
	want := [][]string{{"gh", "pr", "merge", "7", "--squash", "--delete-branch=false"}}
	if got := r.mergeCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge argv changed:\n want %v\n  got %v", want, got)
	}
	for _, c := range r.gh.calls {
		if strings.Contains(argValue(c, "query="), "isInMergeQueue") {
			t.Errorf("a direct merge must not read queue state back: %v", c)
		}
	}
	assertNoAdmin(t, r)

	// The plain interface method keeps its contract too.
	r2 := newQueueRepo(false)
	if err := NewWithRunner(nil, r2.gh.run, nil).MergePR(context.Background(), "/repo", 7, "rebase"); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if got := r2.mergeCalls(); !reflect.DeepEqual(got, [][]string{{"gh", "pr", "merge", "7", "--rebase", "--delete-branch=false"}}) {
		t.Fatalf("configured strategy not passed through: %v", got)
	}
}

// A failing direct merge is still today's error, verbatim.
func TestMerge_NonQueueRepo_FailureUnchanged(t *testing.T) {
	r := newQueueRepo(false)
	r.mergeErr = errors.New("exit status 1")
	r.mergeStderr = "X Pull request is not mergeable: the base branch policy prohibits the merge."
	_, err := NewWithRunner(nil, r.gh.run, nil).MergePRWithOutcome(context.Background(), mergeReq)
	if err == nil || !strings.HasPrefix(err.Error(), "gh pr merge failed: exit status 1\nstderr: X Pull request") {
		t.Fatalf("want today's error, got %v", err)
	}
	if len(r.mergeCalls()) != 1 {
		t.Fatalf("want one merge attempt, got %v", r.mergeCalls())
	}
}

// A queue repo is enqueued with `gh pr merge 7 --auto` (no strategy: the queue
// sets it) and gh's exit 0 is not read as merged.
func TestMerge_QueueRepo_EnqueuesAndReportsQueued(t *testing.T) {
	r := newQueueRepo(true)
	r.onMerge = func(r *queueRepo, _ []string) { r.inQueue = true }
	p := NewWithRunner(nil, r.gh.run, nil)

	outcome, err := p.MergePRWithOutcome(context.Background(), mergeReq)
	if err != nil || outcome != vcs.MergeOutcomeQueued {
		t.Fatalf("want queued/nil, got %v/%v", outcome, err)
	}
	want := [][]string{{"gh", "pr", "merge", "7", "--auto"}}
	if got := r.mergeCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueue argv:\n want %v\n  got %v", want, got)
	}
	assertNoAdmin(t, r)

	// MergePR must never let a caller read an enqueue as merged.
	r2 := newQueueRepo(true)
	r2.autoMerge = true
	if err := NewWithRunner(nil, r2.gh.run, nil).MergePR(context.Background(), "/repo", 7, "squash"); !errors.Is(err, vcs.ErrMergeQueued) {
		t.Fatalf("MergePR on a queue repo: want ErrMergeQueued, got %v", err)
	}
}

// Exit 0 says nothing; the read-back decides. Only MERGED is merged, and a PR
// that shows neither a queue entry nor auto-merge yet is still not merged.
func TestMerge_QueueRepo_OutcomeFromStateNotExitCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(r *queueRepo)
		want    vcs.MergeOutcome
		wantErr bool
	}{
		{"in queue", func(r *queueRepo) { r.inQueue = true }, vcs.MergeOutcomeQueued, false},
		{"auto-merge enabled", func(r *queueRepo) { r.autoMerge = true }, vcs.MergeOutcomeQueued, false},
		{"neither yet", func(*queueRepo) {}, vcs.MergeOutcomeQueued, false},
		{"merged at once", func(r *queueRepo) { r.prState = "MERGED" }, vcs.MergeOutcomeMerged, false},
		{"closed", func(r *queueRepo) { r.prState = "CLOSED" }, vcs.MergeOutcomeQueued, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newQueueRepo(true)
			r.onMerge = func(r *queueRepo, _ []string) { tc.mutate(r) }
			outcome, err := NewWithRunner(nil, r.gh.run, nil).MergePRWithOutcome(context.Background(), mergeReq)
			if (err != nil) != tc.wantErr || outcome != tc.want {
				t.Fatalf("want %v (err %v), got %v/%v", tc.want, tc.wantErr, outcome, err)
			}
		})
	}
}

// The base branch is looked up when the caller has none.
func TestMerge_LooksUpBaseBranch(t *testing.T) {
	r := newQueueRepo(false)
	req := mergeReq
	req.Base = ""
	if _, err := NewWithRunner(nil, r.gh.run, nil).MergePRWithOutcome(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.gh.calls {
		if strings.Contains(argValue(c, "query="), "mergeQueue(branch:") && argValue(c, "branch=") != "main" {
			t.Fatalf("queue lookup must use the PR's base branch, got %v", c)
		}
	}
}

// The answer is cached per repo+branch for the TTL, then asked again — so a
// queue switched on is picked up without a restart.
func TestMerge_QueueDetectionCacheExpires(t *testing.T) {
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	r := newQueueRepo(false)
	p := NewWithRunner(nil, r.gh.run, func() time.Time { return now })

	if _, err := p.MergePRWithOutcome(context.Background(), mergeReq); err != nil {
		t.Fatal(err)
	}
	now = now.Add(mergeQueueTTL - time.Second)
	r.queue = true // switched on, not yet seen
	if _, err := p.MergePRWithOutcome(context.Background(), mergeReq); err != nil {
		t.Fatal(err)
	}
	if r.queueLookups() != 1 {
		t.Fatalf("within the TTL the answer is cached: want 1 lookup, got %d", r.queueLookups())
	}
	other := mergeReq
	other.Base = "release"
	if _, err := p.MergePRWithOutcome(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if r.queueLookups() != 2 {
		t.Fatalf("a different base branch is its own cache entry: want 2 lookups, got %d", r.queueLookups())
	}

	now = now.Add(2 * time.Second) // past the TTL for main
	r.autoMerge = true
	outcome, err := p.MergePRWithOutcome(context.Background(), mergeReq)
	if err != nil || outcome != vcs.MergeOutcomeQueued {
		t.Fatalf("after the TTL the queue must be seen: got %v/%v", outcome, err)
	}
	if r.queueLookups() != 3 {
		t.Fatalf("want a fresh lookup after the TTL, got %d lookups", r.queueLookups())
	}
	calls := r.mergeCalls()
	if last := calls[len(calls)-1]; !reflect.DeepEqual(last, []string{"gh", "pr", "merge", "7", "--auto"}) {
		t.Fatalf("after the TTL the PR must be enqueued, got %v", last)
	}
	assertNoAdmin(t, r)
}

// A lookup that fails is not cached and no merge is attempted on a guess.
func TestMerge_QueueLookupFailureDoesNotMerge(t *testing.T) {
	r := newQueueRepo(false)
	inner := r.gh.handle
	fail := true
	r.gh.handle = func(args []string) (string, string, error) {
		if fail && strings.Contains(argValue(args, "query="), "mergeQueue(branch:") {
			return "", "gh: HTTP 502", errors.New("exit status 1")
		}
		return inner(args)
	}
	p := NewWithRunner(nil, r.gh.run, nil)
	if _, err := p.MergePRWithOutcome(context.Background(), mergeReq); err == nil || !IsTransient(err) {
		t.Fatalf("want a transient lookup error, got %v", err)
	}
	if len(r.mergeCalls()) != 0 {
		t.Fatalf("no merge may run while the queue question is unanswered: %v", r.mergeCalls())
	}
	fail = false
	if _, err := p.MergePRWithOutcome(context.Background(), mergeReq); err != nil {
		t.Fatal(err)
	}
	if r.queueLookups() != 2 {
		t.Fatalf("a failed lookup must not be cached: want 2 lookups, got %d", r.queueLookups())
	}
}

// Fallback: the cache still says "no queue" but GitHub refuses the direct
// merge for one. The Forge re-learns it and enqueues.
func TestMerge_FallbackOnMergeQueueRequiredError(t *testing.T) {
	r := newQueueRepo(false)
	p := NewWithRunner(nil, r.gh.run, nil)
	if _, err := p.MergePRWithOutcome(context.Background(), mergeReq); err != nil {
		t.Fatal(err) // caches "no queue"
	}
	r.queue = true
	r.onMerge = func(r *queueRepo, args []string) {
		if slices.Contains(args, "--squash") {
			r.mergeErr = errors.New("exit status 1")
			r.mergeStderr = "GraphQL: Changes must be made through the merge queue (mergePullRequest)"
			return
		}
		r.mergeErr, r.mergeStderr = nil, ""
		r.inQueue = true
	}
	outcome, err := p.MergePRWithOutcome(context.Background(), mergeReq)
	if err != nil || outcome != vcs.MergeOutcomeQueued {
		t.Fatalf("want queued/nil after the fallback, got %v/%v", outcome, err)
	}
	calls := r.mergeCalls()
	if got := calls[len(calls)-2:]; !reflect.DeepEqual(got, [][]string{
		{"gh", "pr", "merge", "7", "--squash", "--delete-branch=false"},
		{"gh", "pr", "merge", "7", "--auto"},
	}) {
		t.Fatalf("want direct attempt then enqueue, got %v", got)
	}
	// The fallback taught the cache: the next merge goes straight to --auto.
	before := r.queueLookups()
	if _, err := p.MergePRWithOutcome(context.Background(), mergeReq); err != nil {
		t.Fatal(err)
	}
	calls = r.mergeCalls()
	if last := calls[len(calls)-1]; !slices.Contains(last, "--auto") || r.queueLookups() != before {
		t.Fatalf("after the fallback the cached answer is queue: got %v, lookups %d->%d", last, before, r.queueLookups())
	}
	assertNoAdmin(t, r)
}

// gh enqueues even when given --squash on a queue branch, exiting 0. If its
// output says so, the outcome is read from the PR, not assumed merged.
func TestMerge_DirectMergeThatGhEnqueuedIsQueued(t *testing.T) {
	r := newQueueRepo(false)
	r.mergeStderr = "! The merge strategy for main is set by the merge queue\n"
	r.onMerge = func(r *queueRepo, _ []string) { r.inQueue = true }
	outcome, err := NewWithRunner(nil, r.gh.run, nil).MergePRWithOutcome(context.Background(), mergeReq)
	if err != nil || outcome != vcs.MergeOutcomeQueued {
		t.Fatalf("want queued, got %v/%v", outcome, err)
	}
}

// The batched poll carries the queue fields, so a queued PR resolves on the
// regular poll with no extra request; a node without them stays "unknown".
func TestCheckStatusBatch_CarriesMergeQueueState(t *testing.T) {
	queued := strings.Replace(node(31), `"number":31,`, `"number":31,"isInMergeQueue":true,"autoMergeRequest":{"enabledAt":"x"},"mergeQueueEntry":{"state":"AWAITING_CHECKS"},`, 1)
	dequeued := strings.Replace(node(32), `"number":32,`, `"number":32,"isInMergeQueue":false,"autoMergeRequest":null,"mergeQueueEntry":null,`, 1)
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		if !strings.Contains(argValue(args, "query="), "isInMergeQueue autoMergeRequest { enabledAt } mergeQueueEntry { state }") {
			return "", "batch query lacks the merge-queue fields", errors.New("exit status 1")
		}
		return okHeaders + batchBody(map[int]string{31: queued, 32: dequeued, 33: node(33)}), "", nil
	}}
	got, err := NewWithRunner(nil, f.run, nil).CheckStatusBatch(context.Background(), "/repo", []int{31, 32, 33})
	if err != nil {
		t.Fatal(err)
	}
	if f.ghCalls() != 1 {
		t.Fatalf("want one request, got %d", f.ghCalls())
	}
	if mq := got[31].MergeQueue; mq == nil || !mq.InQueue || !mq.AutoMergeEnabled || mq.EntryState != "AWAITING_CHECKS" {
		t.Errorf("queued PR: %+v", got[31].MergeQueue)
	}
	if mq := got[32].MergeQueue; mq == nil || mq.InQueue || mq.AutoMergeEnabled {
		t.Errorf("dequeued PR: %+v", got[32].MergeQueue)
	}
	if got[33].MergeQueue != nil {
		t.Errorf("a node without the fields must read as unknown, got %+v", got[33].MergeQueue)
	}
}

func TestMergeGroupFailure_PicksThisPRsFailedRun(t *testing.T) {
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		return `[{"headBranch":"gh-readonly-queue/main/pr-70-aaa","conclusion":"failure","workflowName":"CI","url":"u70"},
		         {"headBranch":"gh-readonly-queue/main/pr-7-bbb","conclusion":"success","workflowName":"CI","url":"ok"},
		         {"headBranch":"gh-readonly-queue/main/pr-7-ccc","conclusion":"failure","workflowName":"CI","url":"u7"}]`, "", nil
	}}
	got, err := NewWithRunner(nil, f.run, nil).MergeGroupFailure(context.Background(), "/repo", 7)
	if err != nil || got != `merge_group run "CI" failure: u7` {
		t.Fatalf("got %q / %v", got, err)
	}
}
