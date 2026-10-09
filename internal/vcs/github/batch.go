package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/Robin831/Forge/internal/vcs"
)

// batchChunkSize bounds how many PRs share one GraphQL query, keeping each
// request's node count (and so its rate-limit cost) predictable.
const batchChunkSize = 25

// batchPRFields selects what CheckStatus gets from `gh pr view --json
// state,statusCheckRollup,reviews,reviewRequests,mergeable,reviewDecision,
// mergeStateStatus,headRefName,headRefOid,isDraft,url,title` plus the thread and review-request queries.
// The statusCheckRollup and reviews selections mirror gh's own, including
// gh's export of a StatusContext's createdAt as startedAt. The merge-queue
// fields let a queued PR resolve on the regular poll at no extra request.
const batchPRFields = `number state mergeable reviewDecision mergeStateStatus headRefName headRefOid isDraft url title ` + mergeQueueFields + `
	statusCheckRollup: commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) {
		pageInfo { hasNextPage endCursor }
		nodes { __typename
			... on CheckRun { name status conclusion startedAt completedAt }
			... on StatusContext { context state startedAt: createdAt }
		} } } } } }
	reviews(first: 100) { pageInfo { hasNextPage endCursor } nodes { author { login } state body } }
	reviewThreads(first: 100) { pageInfo { hasNextPage endCursor } nodes { isResolved } }
	` + reviewRequestsSelection

type batchPR struct {
	Number    int    `json:"number"`
	State     string `json:"state"`
	Mergeable string `json:"mergeable"`
	// ReviewDecision is null when no review is required; that decodes to "".
	ReviewDecision   string `json:"reviewDecision"`
	MergeStateStatus string `json:"mergeStateStatus"`
	HeadRefName      string `json:"headRefName"`
	HeadRefOid       string `json:"headRefOid"`
	IsDraft          bool   `json:"isDraft"`
	URL              string `json:"url"`
	Title            string `json:"title"`
	Rollup           struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						PageInfo pageInfo       `json:"pageInfo"`
						Nodes    []vcs.CheckRun `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"statusCheckRollup"`
	Reviews struct {
		PageInfo pageInfo     `json:"pageInfo"`
		Nodes    []vcs.Review `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads  threadPage        `json:"reviewThreads"`
	ReviewRequests reviewRequestConn `json:"reviewRequests"`
	mergeQueueNode
}

// CheckStatusBatch fetches the status of every PR in prNumbers with one
// GraphQL request per batchChunkSize PRs, instead of CheckStatus's three
// requests per PR. A PR with more than one page of review threads has the
// rest counted per PR; a PR with more than 100 check contexts or reviews, or
// one GitHub could not resolve, is left out for the caller's CheckStatus.
//
// A rate-limit refusal returns a *RateLimitError carrying the Retry-After or
// x-ratelimit-reset hint, read from the response headers (`gh api --include`).
func (p *Provider) CheckStatusBatch(ctx context.Context, worktreePath string, prNumbers []int) (map[int]*vcs.PRStatus, error) {
	out := make(map[int]*vcs.PRStatus, len(prNumbers))
	if len(prNumbers) == 0 {
		return out, nil
	}
	owner, repo, err := p.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(prNumbers); start += batchChunkSize {
		end := min(start+batchChunkSize, len(prNumbers))
		if err := p.checkStatusChunk(ctx, worktreePath, owner, repo, prNumbers[start:end], out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (p *Provider) checkStatusChunk(ctx context.Context, worktreePath, owner, repo string, prNumbers []int, out map[int]*vcs.PRStatus) error {
	var q strings.Builder
	q.WriteString("query($owner:String!, $repo:String!) { repository(owner:$owner, name:$repo) {\n")
	for _, n := range prNumbers {
		fmt.Fprintf(&q, "pr%d: pullRequest(number: %d) { ...prStatus }\n", n, n)
	}
	q.WriteString("} }\nfragment prStatus on PullRequest { " + batchPRFields + " }")

	stdout, stderr, runErr := p.run(ctx, worktreePath, "gh",
		"api", "graphql", "--include",
		"-f", "query="+q.String(),
		"-f", "owner="+owner,
		"-f", "repo="+repo,
	)
	cause := fmt.Errorf("gh api graphql (batch of %d PRs): %v\nstderr: %s", len(prNumbers), runErr, stderr)
	code, hdr, body, included := splitIncludedResponse(stdout)
	if included {
		if rl := rateLimitFromResponse(code, hdr, body, p.nowFn(), cause); rl != nil {
			return rl
		}
	}

	var resp struct {
		Data struct {
			Repository map[string]*batchPR `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Data.Repository == nil {
		if runErr != nil {
			return cause
		}
		return fmt.Errorf("parsing batch graphql response: %v", err)
	}
	// gh exits non-zero when GraphQL reports any error, e.g. one PR number that
	// does not resolve; the rest of the data is still good.
	if runErr != nil {
		log.Printf("[vcs/github] Warning: batch status query reported errors; using the PRs it did return: %s", strings.TrimSpace(string(stderr)))
	}

	for _, n := range prNumbers {
		pr := resp.Data.Repository[fmt.Sprintf("pr%d", n)]
		if pr == nil {
			continue
		}
		status, complete := pr.status()
		if !complete {
			continue
		}
		if pr.ReviewThreads.PageInfo.HasNextPage {
			count, err := p.countUnresolvedThreads(ctx, worktreePath, owner, repo, n,
				pr.ReviewThreads.PageInfo.EndCursor, status.UnresolvedThreads)
			if err != nil {
				status.UnresolvedThreads = 0
				status.UnresolvedThreadsUnknown = true
				log.Printf("[vcs/github] Warning: could not fetch unresolved thread count for PR #%d: %v", n, err)
			} else {
				status.UnresolvedThreads = count
			}
		}
		out[n] = status
	}
	return nil
}

// status converts the batch node into the PRStatus CheckStatus would return.
// complete is false when the check contexts or reviews ran past one page.
func (b *batchPR) status() (*vcs.PRStatus, bool) {
	if b.Reviews.PageInfo.HasNextPage {
		return nil, false
	}
	var rollup []vcs.CheckRun
	for _, node := range b.Rollup.Nodes {
		scr := node.Commit.StatusCheckRollup
		if scr == nil {
			continue
		}
		if scr.Contexts.PageInfo.HasNextPage {
			return nil, false
		}
		rollup = append(rollup, scr.Contexts.Nodes...)
	}
	return &vcs.PRStatus{
		State:             b.State,
		StatusCheckRollup: rollup,
		Reviews:           b.Reviews.Nodes,
		ReviewRequests:    b.ReviewRequests.requests(),
		Mergeable:         b.Mergeable,
		ReviewDecision:    b.ReviewDecision,
		MergeStateStatus:  b.MergeStateStatus,
		UnresolvedThreads: b.ReviewThreads.unresolved(),
		HeadRefName:       b.HeadRefName,
		HeadSHA:           b.HeadRefOid,
		IsDraft:           b.IsDraft,
		URL:               b.URL,
		Title:             b.Title,
		MergeQueue:        b.info(),
	}, true
}
