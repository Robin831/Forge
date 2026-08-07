# Burnish (Review Fix) Loop and Re-Review Mechanism

The Forge automatically responds to PR review comments from GitHub Copilot (and
other reviewers) by spawning a Smith agent to address them, then requesting a
fresh review once the fixes are pushed.

## Flow

```
Bellows detects "changes requested" or unresolved threads
    ↓
burnish.Fix() fetches review comments via GraphQL
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
(default 2m; minimum 30 s). It fires an `EventReviewChanges` event when:

- A review transitions to `CHANGES_REQUESTED`, or
- The count of unresolved review threads increases from zero.

The daemon's event handler for `EventReviewChanges` calls `burnish.Fix()`.

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

## Verification and the Unverified Push

Between Smith's commit and the push, Burnish runs Temper (`burnish_verify_timeout`,
default 5m). Unlike the pipeline's Temper, **this verification is advisory**: every
Burnish output lands on an open PR that humans, Copilot and Assay all review again,
so it is a sanity check rather than the gate.

That distinction decides what happens when verification does not finish:

```
verification times out
    ↓
re-run it (burnish_verify_retries, default 1)  ── completes → normal path
    ↓ still timing out
push the commit anyway, marked UNVERIFIED
    ↓ push succeeded                    ↓ push failed
event review_fix_unverified_push        event review_fix_work_preserved
Needs Attention entry naming the SHA    worktree KEPT, SHA + path named
                                        in the Needs Attention entry
```

Burnish never loops back to Smith on a timeout: a timeout says nothing about the
diff, so another attempt would rebuild identical work.

**A worktree whose HEAD is not on the remote is never deleted.** Teardown for the
review-fix path goes through `worktree.RemoveIfPushed`, which proves the commit is
reachable from the remote (an ancestor of `origin/<branch>`, or contained in some
other remote-tracking branch) before removing anything. Anything it cannot prove is
kept, and the commit and checkout are named in a Needs Attention entry — recovery is
a `git push` or cherry-pick rather than an excavation of `git fsck --lost-found`.

This replaced the original behaviour, where a verification timeout logged a WARN,
skipped the push, deleted the worktree and reported the cycle complete: a finished,
correct fix commit survived only as an unreferenced object while the operator saw a
success table (Forge-xl50).

## Retry Limits

Several settings control how many automated fix cycles run before a PR is flagged as needing human attention (`needs_human=1` in the state DB):

| Setting | Default | Description |
|---------|---------|-------------|
| `max_review_attempts` | `2` | Max Warden review iterations during the initial Smith pipeline. |
| `max_review_fix_attempts` | `5` | Max review fix cycles per PR (Bellows-driven, post-PR-creation review comments), for the PR's whole life. |
| `max_same_head_review_fixes` | `2` | Max review fix cycles against **one unchanged PR head**. See below. |
| `max_ci_fix_attempts` | `5` | Max CI fix cycles per PR when CI fails after creation. |
| `max_rebase_attempts` | `3` | Max conflict rebase attempts per PR before marking as exhausted. |

### The same-head circuit breaker

`max_review_fix_attempts` never resets, so it treats a PR that is progressing
(each round pushes a new head and addresses new comments) exactly like one that
rebuilds the identical diff every Bellows cycle. The second is the expensive
failure — one full Smith run per cycle, producing the same commit every time —
and an unchanged head is precisely what identifies it.

So each dispatch is recorded against the PR's current head SHA
(`review_fix_dispatches` in the state DB). Past `max_same_head_review_fixes`
against an unchanged head, the dispatch is refused and a Needs Attention entry
names the PR, head SHA, attempt count and how the previous attempts ended
(`pushed` / `unverified_push` / `preserved` / `failed`).

- The counter **resets the moment the head moves** — a progressing PR is never broken.
- `forge queue retry` clears it along with the other fix counters.
- A manual review fix (Hearth's "fix comments") bypasses it: the breaker is what
  the operator is overriding.
- It **fails open**: if the head SHA cannot be read, the dispatch proceeds. A
  breaker that blocks fixes because `gh` was briefly unreachable would be worse
  than the loop it prevents.

## Configuration

```yaml
settings:
  max_review_attempts: 2         # Warden iterations during initial pipeline
  max_review_fix_attempts: 5     # Post-PR review fix cycles (PR lifetime)
  max_same_head_review_fixes: 2  # Post-PR review fix cycles against one unchanged head
  max_ci_fix_attempts: 5         # Post-PR CI fix cycles
  max_rebase_attempts: 3         # Conflict rebase attempts
  burnish_verify_timeout: 5m     # Temper deadline within one review-fix attempt
  burnish_verify_retries: 1      # Extra verification runs after a timeout
  bellows_interval: 2m           # How often Bellows polls for PR status changes
```

To use a different reviewer (e.g. a human or another bot), supply
`FixParams.Reviewer` directly when calling `burnish.Fix()`.

## State Events

| Event | Meaning |
|-------|---------|
| `review_changes` | Bellows detected changes-requested or new unresolved threads |
| `review_fix_started` | Fix cycle started (Smith about to run) |
| `review_fix_success` | Smith fixed the comments and pushed |
| `re_review_requested` | Re-review requested from the configured reviewer |
| `re_review_request_failed` | Re-review request failed (e.g. GitHub or CLI/API error) |
| `review_fix_failed` | A fix cycle failed (e.g., Smith error during the fix phase) |
| `review_fix_temper_failed` | Verification ran and failed; the fix was not pushed |
| `review_fix_unverified_push` | Verification never completed; the fix was pushed marked unverified |
| `review_fix_work_preserved` | A fix commit could not be pushed; its worktree was kept so the commit is recoverable |
| `review_fix_circuit_broken` | A dispatch was refused because the PR head has not moved across the attempt budget |
| `review_thread_resolved` | An individual review thread was resolved on GitHub |
