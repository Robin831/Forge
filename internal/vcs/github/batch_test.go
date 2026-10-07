package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/vcs"
)

// fakeGH is a Runner that answers gh/git invocations from canned responses and
// records every call, so tests can count GitHub requests without a network.
type fakeGH struct {
	calls  [][]string
	handle func(args []string) (stdout, stderr string, err error)
}

func (f *fakeGH) run(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "git" {
		return []byte("https://github.com/owner/repo.git\n"), nil, nil
	}
	out, errOut, err := f.handle(args)
	return []byte(out), []byte(errOut), err
}

func (f *fakeGH) ghCalls() int {
	n := 0
	for _, c := range f.calls {
		if c[0] == "gh" {
			n++
		}
	}
	return n
}

func argValue(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix)
		}
	}
	return ""
}

func isBatch(args []string) bool {
	return strings.Contains(argValue(args, "query="), "fragment prStatus")
}

const okHeaders = "HTTP/2.0 200 OK\nContent-Type: application/json\r\nX-Ratelimit-Remaining: 4999\r\n\r\n"

// prNode is one PR as the batch query returns it; the same facts are served
// to the per-PR path below in gh's own export shape.
const prNode = `{"number":%d,"state":"OPEN","mergeable":"CONFLICTING","headRefName":"forge/b%d","headRefOid":"sha%d","isDraft":true,"url":"https://github.com/owner/repo/pull/%d","title":"PR %d",
 "statusCheckRollup":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":"c"},"nodes":[
   {"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:05:00Z"},
   {"__typename":"StatusContext","context":"ci/legacy","state":"PENDING","startedAt":"2026-01-01T00:01:00Z"}]}}}}]},
 "reviews":{"pageInfo":{"hasNextPage":false,"endCursor":"r"},"nodes":[{"author":{"login":"alice"},"state":"APPROVED","body":"lgtm"},{"author":{"login":"bob"},"state":"CHANGES_REQUESTED","body":"fix"}]},
 "reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"t"},"nodes":[{"isResolved":false},{"isResolved":true},{"isResolved":false}]},
 "reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"Bot","login":"copilot-pull-request-reviewer"}},{"requestedReviewer":{"__typename":"Team","slug":"core","name":"Core"}}]}}`

// ghPRView is the same PR as `gh pr view --json ...` exports it.
const ghPRView = `{"headRefName":"forge/b%d","headRefOid":"sha%d","isDraft":true,"mergeable":"CONFLICTING","reviewRequests":[],
 "reviews":[{"id":"","author":{"login":"alice"},"authorAssociation":"","body":"lgtm","submittedAt":null,"state":"APPROVED","commit":{"oid":""}},{"id":"","author":{"login":"bob"},"body":"fix","state":"CHANGES_REQUESTED"}],
 "state":"OPEN","statusCheckRollup":[
   {"__typename":"CheckRun","completedAt":"2026-01-01T00:05:00Z","conclusion":"FAILURE","detailsUrl":"d","name":"build","startedAt":"2026-01-01T00:00:00Z","status":"COMPLETED","workflowName":"CI"},
   {"__typename":"StatusContext","context":"ci/legacy","startedAt":"2026-01-01T00:01:00Z","state":"PENDING","targetUrl":"t"}],
 "title":"PR %d","url":"https://github.com/owner/repo/pull/%d"}`

const threadsPage = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"t"},"nodes":[{"isResolved":false},{"isResolved":true},{"isResolved":false}]}}}}}`
const reviewRequestsResp = `{"data":{"repository":{"pullRequest":{"reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"Bot","login":"copilot-pull-request-reviewer"}},{"requestedReviewer":{"__typename":"Team","slug":"core","name":"Core"}}]}}}}}`

func batchBody(nodes map[int]string) string {
	var parts []string
	for n, node := range nodes {
		parts = append(parts, fmt.Sprintf(`"pr%d":%s`, n, node))
	}
	return `{"data":{"repository":{` + strings.Join(parts, ",") + `}}}`
}

func node(n int) string { return fmt.Sprintf(prNode, n, n, n, n, n) }

// TestCheckStatusBatch_SameStatusAsCheckStatus pins the contract bellows relies
// on: the batch returns, field for field, the PRStatus CheckStatus builds from
// `gh pr view` plus its thread and review-request queries — in one request for
// every PR instead of three per PR.
func TestCheckStatusBatch_SameStatusAsCheckStatus(t *testing.T) {
	perPR := &fakeGH{handle: func(args []string) (string, string, error) {
		switch {
		case args[0] == "pr" && args[1] == "view":
			var n int
			fmt.Sscanf(args[2], "%d", &n)
			return fmt.Sprintf(ghPRView, n, n, n, n), "", nil
		case strings.Contains(argValue(args, "query="), "reviewThreads"):
			return threadsPage, "", nil
		case strings.Contains(argValue(args, "query="), "reviewRequests"):
			return reviewRequestsResp, "", nil
		}
		return "", "unexpected call", errors.New("exit status 1")
	}}
	batch := &fakeGH{handle: func(args []string) (string, string, error) {
		if !isBatch(args) {
			return "", "unexpected call", errors.New("exit status 1")
		}
		return okHeaders + batchBody(map[int]string{11: node(11), 12: node(12), 13: node(13)}), "", nil
	}}

	pp := NewWithRunner(nil, perPR.run, nil)
	bp := NewWithRunner(nil, batch.run, nil)
	got, err := bp.CheckStatusBatch(context.Background(), "/repo", []int{11, 12, 13})
	if err != nil {
		t.Fatalf("CheckStatusBatch: %v", err)
	}
	for _, n := range []int{11, 12, 13} {
		want, err := pp.CheckStatus(context.Background(), "/repo", n)
		if err != nil {
			t.Fatalf("CheckStatus(%d): %v", n, err)
		}
		if !reflect.DeepEqual(want, got[n]) {
			t.Errorf("PR %d: batch status differs from CheckStatus\n want %+v\n  got %+v", n, want, got[n])
		}
	}
	if perPR.ghCalls() != 9 {
		t.Errorf("per-PR path: want 3 gh calls per PR (9), got %d", perPR.ghCalls())
	}
	if batch.ghCalls() != 1 {
		t.Errorf("batch path: want 1 gh call for 3 PRs, got %d", batch.ghCalls())
	}
}

func TestCheckStatusBatch_ChunksLargeSets(t *testing.T) {
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		nodes := map[int]string{}
		q := argValue(args, "query=")
		for n := 1; n <= 60; n++ {
			if strings.Contains(q, fmt.Sprintf("pr%d:", n)) {
				nodes[n] = node(n)
			}
		}
		return okHeaders + batchBody(nodes), "", nil
	}}
	var nums []int
	for n := 1; n <= 60; n++ {
		nums = append(nums, n)
	}
	got, err := NewWithRunner(nil, f.run, nil).CheckStatusBatch(context.Background(), "/repo", nums)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 60 || f.ghCalls() != 3 {
		t.Fatalf("want 60 statuses in 3 requests of <=%d, got %d in %d", batchChunkSize, len(got), f.ghCalls())
	}
}

// A PR with more than one page of review threads keeps an exact count: the
// batch resumes the thread pagination from its cursor for that PR only.
func TestCheckStatusBatch_ThreadOverflowResumesFromCursor(t *testing.T) {
	big := strings.Replace(node(21), `"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"t"}`,
		`"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"CUR1"}`, 1)
	var cursorSeen string
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		if isBatch(args) {
			return okHeaders + batchBody(map[int]string{21: big, 22: node(22)}), "", nil
		}
		cursorSeen = argValue(args, "cursor=")
		return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"isResolved":false},{"isResolved":false},{"isResolved":true}]}}}}}`, "", nil
	}}
	got, err := NewWithRunner(nil, f.run, nil).CheckStatusBatch(context.Background(), "/repo", []int{21, 22})
	if err != nil {
		t.Fatal(err)
	}
	if cursorSeen != "CUR1" {
		t.Errorf("continuation must start at the batch's endCursor, got %q", cursorSeen)
	}
	if got[21].UnresolvedThreads != 4 || got[22].UnresolvedThreads != 2 {
		t.Errorf("thread counts: want 4 (2+2) and 2, got %d and %d", got[21].UnresolvedThreads, got[22].UnresolvedThreads)
	}
	if f.ghCalls() != 2 {
		t.Errorf("want 1 batch + 1 continuation, got %d gh calls", f.ghCalls())
	}
}

func TestCheckStatusBatch_ThreadOverflowFailureIsUnknown(t *testing.T) {
	big := strings.Replace(node(21), `"hasNextPage":false,"endCursor":"t"`, `"hasNextPage":true,"endCursor":"CUR1"`, 1)
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		if isBatch(args) {
			return okHeaders + batchBody(map[int]string{21: big}), "", nil
		}
		return "", "gh: HTTP 502", errors.New("exit status 1")
	}}
	got, err := NewWithRunner(nil, f.run, nil).CheckStatusBatch(context.Background(), "/repo", []int{21})
	if err != nil {
		t.Fatal(err)
	}
	if !got[21].UnresolvedThreadsUnknown || got[21].UnresolvedThreads != 0 {
		t.Errorf("a failed continuation must read as unknown, not as a partial count: %+v", got[21])
	}
}

// PRs the batch cannot answer completely are left out for CheckStatus: more
// than 100 check contexts or reviews, or a number GitHub cannot resolve (gh
// exits non-zero, but the rest of the data is still used).
func TestCheckStatusBatch_LeavesOutWhatItCannotAnswer(t *testing.T) {
	manyChecks := strings.Replace(node(31), `"contexts":{"pageInfo":{"hasNextPage":false`, `"contexts":{"pageInfo":{"hasNextPage":true`, 1)
	manyReviews := strings.Replace(node(32), `"reviews":{"pageInfo":{"hasNextPage":false`, `"reviews":{"pageInfo":{"hasNextPage":true`, 1)
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		body := `{"data":{"repository":{"pr31":` + manyChecks + `,"pr32":` + manyReviews + `,"pr33":` + node(33) + `,"pr34":null}},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a PullRequest with the number of 34."}]}`
		return okHeaders + body, "gh: Could not resolve to a PullRequest with the number of 34.", errors.New("exit status 1")
	}}
	got, err := NewWithRunner(nil, f.run, nil).CheckStatusBatch(context.Background(), "/repo", []int{31, 32, 33, 34})
	if err != nil {
		t.Fatalf("a partial GraphQL error must not fail the batch: %v", err)
	}
	if len(got) != 1 || got[33] == nil {
		t.Fatalf("want only PR 33, got %v", got)
	}
}

func TestCheckStatusBatch_TransportFailureIsClassified(t *testing.T) {
	f := &fakeGH{handle: func(args []string) (string, string, error) {
		return "HTTP/2.0 502 Bad Gateway\nContent-Type: text/html\r\n\r\n<html>bad gateway</html>", "gh: HTTP 502: Bad Gateway", errors.New("exit status 1")
	}}
	_, err := NewWithRunner(nil, f.run, nil).CheckStatusBatch(context.Background(), "/repo", []int{1})
	if err == nil || !IsTransient(err) {
		t.Fatalf("a 502 must surface as a transient error, got %v", err)
	}
	if _, ok := AsRateLimit(err); ok {
		t.Fatalf("a 502 is not a rate limit")
	}
}

func TestCheckStatusBatch_RateLimitCarriesServerHint(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		stdout        string
		wantSecondary bool
		wantAfter     time.Duration
	}{
		{
			name: "secondary 403 with Retry-After",
			stdout: "HTTP/2.0 403 Forbidden\nContent-Type: application/json\r\nRetry-After: 37\r\nX-Ratelimit-Remaining: 4000\r\n\r\n" +
				`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			wantSecondary: true, wantAfter: 37 * time.Second,
		},
		{
			name: "primary 403 with x-ratelimit-reset",
			stdout: fmt.Sprintf("HTTP/2.0 403 Forbidden\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: %d\r\n\r\n", now.Add(10*time.Minute).Unix()) +
				`{"message":"API rate limit exceeded for user ID 1."}`,
			wantSecondary: false, wantAfter: 10 * time.Minute,
		},
		{
			name:          "429 without a hint",
			stdout:        "HTTP/2.0 429 Too Many Requests\nContent-Type: application/json\r\n\r\n{\"message\":\"slow down\"}",
			wantSecondary: true, wantAfter: 0,
		},
		{
			name: "GraphQL RATE_LIMITED on a 200",
			stdout: fmt.Sprintf("HTTP/2.0 200 OK\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: %d\r\n\r\n", now.Add(90*time.Second).Unix()) +
				`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`,
			wantSecondary: false, wantAfter: 90 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGH{handle: func([]string) (string, string, error) {
				return tc.stdout, "gh: rate limited", errors.New("exit status 1")
			}}
			_, err := NewWithRunner(nil, f.run, func() time.Time { return now }).CheckStatusBatch(context.Background(), "/repo", []int{1, 2})
			rl, ok := AsRateLimit(err)
			if !ok {
				t.Fatalf("want a rate-limit error, got %v", err)
			}
			var typed *RateLimitError
			if !errors.As(err, &typed) {
				t.Fatalf("want a typed *RateLimitError, got %T", err)
			}
			if rl.Secondary != tc.wantSecondary || rl.RetryAfter != tc.wantAfter {
				t.Errorf("got secondary=%v after=%s, want secondary=%v after=%s", rl.Secondary, rl.RetryAfter, tc.wantSecondary, tc.wantAfter)
			}
			if !IsTransient(err) {
				t.Errorf("a rate limit stays transient for callers that only ask IsTransient")
			}
			if f.ghCalls() != 1 {
				t.Errorf("the batch must not re-issue a refused request itself, got %d calls", f.ghCalls())
			}
		})
	}
}

// The exact stdout gh 2.102 prints for `gh api graphql --include` against a
// 403 (captured from a local httptest server).
func TestSplitIncludedResponse_RealGHOutput(t *testing.T) {
	out := "HTTP/1.1 403 Forbidden\nContent-Length: 149\r\nContent-Type: application/json\r\nDate: Wed, 07 Oct 2026 06:49:38 GMT\r\nRetry-After: 37\r\nX-Ratelimit-Remaining: 4000\r\n\r\n{\"message\":\"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.\",\"documentation_url\":\"https://docs.github.com\"}"
	code, hdr, body, ok := splitIncludedResponse([]byte(out))
	if !ok || code != 403 || hdr.Get("Retry-After") != "37" || hdr.Get("x-ratelimit-remaining") != "4000" {
		t.Fatalf("parse: ok=%v code=%d hdr=%v", ok, code, hdr)
	}
	if !strings.HasPrefix(string(body), `{"message":"You have exceeded`) {
		t.Fatalf("body: %q", body)
	}
	if _, _, _, ok := splitIncludedResponse([]byte(`{"data":{}}`)); ok {
		t.Fatalf("output without a status line must not parse as a response")
	}
}

var _ vcs.BatchStatusChecker = (*Provider)(nil)
