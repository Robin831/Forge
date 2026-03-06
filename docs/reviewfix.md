# Reviewfix Loop and Re-Review Mechanism

The Forge automatically responds to PR review comments from GitHub Copilot (and
other reviewers) by spawning a Smith agent to address them, then requesting a
fresh review once the fixes are pushed.

## Flow

```
Bellows detects "changes requested" or unresolved threads
    ↓
reviewfix.Fix() fetches review comments via GraphQL
    ↓
Smith spawned with targeted fix prompt
    ↓
Smith commits and pushes fixes to the PR branch
    ↓
Resolved threads are marked resolved on GitHub
    ↓
gh pr edit <pr> --add-reviewer copilot-pull-request-reviewer
    ↓
Copilot re-reviews the updated PR
    ↓
(if approved) Bellows detects approval → PR can be merged
    (if still changes requested) loop repeats up to MaxAttempts
```

## Triggering

Bellows (`internal/bellows`) polls all open PRs on a configurable interval
(default 5m; minimum 30 s). It fires an `EventReviewChanges` event when:

- A review transitions to `CHANGES_REQUESTED`, or
- The count of unresolved review threads increases from zero.

The daemon's event handler for `EventReviewChanges` calls `reviewfix.Fix()`.

## Re-Review Request

After `Fix()` successfully pushes review fixes it calls
`ghpr.RequestReReview()`, which runs:

```
gh pr edit <PR number> --add-reviewer copilot-pull-request-reviewer
```

This notifies GitHub Copilot (or any configured reviewer) to re-examine the PR.
Without this step the reviewer is never prompted and the review cycle stalls.

The reviewer handle is configurable via `FixParams.Reviewer`. When empty it
defaults to `copilot-pull-request-reviewer` (the handle used by GitHub Copilot
Code Review).

## Retry Limit

`FixParams.MaxAttempts` (configurable in `forge.yaml` via
`settings.max_review_attempts`, default 2) caps the number of fix cycles per
PR. After exhausting attempts, the PR is flagged `needs_human=1` in the state
DB so the operator is alerted.

## Configuration

```yaml
settings:
  max_review_attempts: 2   # max fix+re-review cycles per PR
```

To use a different reviewer (e.g. a human or another bot), supply
`FixParams.Reviewer` directly when calling `reviewfix.Fix()`.

## State Events

| Event | Meaning |
|-------|---------|
| `review_changes` | Bellows detected changes-requested or new unresolved threads |
| `review_fix_started` | Fix cycle started (Smith about to run) |
| `review_fix_success` | Smith fixed the comments and pushed |
| `re_review_requested` | Re-review requested from the configured reviewer |
| `re_review_request_failed` | Re-review request failed (e.g. GitHub or CLI/API error) |
| `review_fix_failed` | A fix cycle failed (e.g., Smith error during the fix phase) |
| `review_thread_resolved` | An individual review thread was resolved on GitHub |
