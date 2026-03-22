# Wicket: GitHub Issues Intake Worker

## 1. Naming and Concept

**Name: Wicket**

In a real blacksmith's shop, the **wicket** (or wicket gate) is the small door or window in the front of the shop where customers place their orders and requests. The blacksmith's apprentice would receive work orders through the wicket, triage them, and decide which ones to bring to the smith. This maps perfectly to a worker that receives GitHub issues (customer requests) and triages them into actionable beads.

You might say this feature really... *opens doors* for the project. (Sorry, that one's a freebie.)

**Package:** `internal/wicket`

**Concept:** The Wicket monitors GitHub issues on configured repositories, uses an AI agent to triage incoming issues, and either creates beads, requests clarification, rejects them, or flags them for human review. It bridges the gap between "user writes a GitHub issue" and "Forge autonomously fixes it."

---

## 2. Configuration Schema (forge.yaml additions)

### Global Settings

```yaml
settings:
  # Master toggle for the Wicket worker (default: false, opt-in)
  wicket_enabled: true

  # How often Wicket polls GitHub for new/updated issues (default: 5m)
  wicket_interval: 5m

  # AI provider for triage decisions. Falls back to settings.providers if empty.
  # Recommend a fast/cheap model since triage is lightweight.
  wicket_provider: "copilot/claude-haiku-4-5"

  # Maximum issues to process per poll cycle, to avoid rate limit storms (default: 10)
  wicket_batch_size: 10

  # GitHub label applied to issues that Wicket has processed (default: "forge-triaged")
  wicket_processed_label: "forge-triaged"

  # GitHub label applied to issues needing human review (default: "forge-needs-human")
  wicket_needs_human_label: "forge-needs-human"

  # GitHub label applied when a bead has been created (default: "forge-bead-created")
  wicket_bead_created_label: "forge-bead-created"

  # Label used to explicitly opt an issue into Wicket processing.
  # When set, only issues with this label are processed (pull model).
  # When empty (default), all new issues from trusted users are processed (push model).
  wicket_trigger_label: ""
```

### Per-Anvil Settings

```yaml
anvils:
  my-repo:
    path: C:\source\my-repo

    # Enable/disable Wicket for this anvil (overrides global; nil = use global)
    wicket_enabled: true

    # Trusted users whose issues get full AI triage (list of GitHub usernames)
    wicket_trusted_users:
      - "robin831"
      - "alice"
      - "bob"

    # Whether to auto-dispatch beads created from trusted user issues
    # (default: false -- ask user for confirmation first)
    wicket_auto_dispatch: false

    # Issue label filter: only process issues with ALL of these labels.
    # Empty means process all issues (subject to user filtering).
    wicket_issue_labels: []

    # Repos to monitor. When empty, monitors the anvil's own repo.
    # Allows monitoring external repos and creating beads in this anvil.
    wicket_repos:
      - "owner/repo"

    # Custom triage prompt appended to the AI triage context.
    # Useful for repo-specific guidance (e.g., "issues about X should be rejected").
    wicket_triage_prompt: ""
```

### Example Minimal Config

```yaml
settings:
  wicket_enabled: true
  wicket_interval: 5m

anvils:
  Forge:
    path: C:\source\Forge
    wicket_enabled: true
    wicket_trusted_users:
      - "robin831"
```

---

## 3. Architecture

### How It Fits Into the Daemon Loop

Wicket follows the same pattern as `depcheck`, `vulncheck`, and `questgiver`: a background monitor with its own goroutine, poll interval, and `UpdateAnvilPaths` method for hot-reload.

```
daemon.go startup:
  if wicket_enabled:
    d.wicketMonitor = wicket.New(d.db, interval, anvilConfigs, provider)
    go d.wicketMonitor.Run(ctx)

daemon.go hot-reload:
  if d.wicketMonitor != nil:
    d.wicketMonitor.UpdateConfig(newAnvilConfigs)
```

### Component Interactions

```
                    GitHub Issues API
                          |
                          v
                  +---------------+
                  |    Wicket     |  (internal/wicket)
                  |   Monitor    |
                  +-------+-------+
                          |
              +-----------+-----------+
              |           |           |
              v           v           v
        Triage Agent   state.db    GitHub API
        (AI provider)  (tracking)  (comments/labels)
              |
              v
         bd create
         (bead creation)
```

### Key Types

```go
package wicket

// Monitor polls GitHub issues and triages them.
type Monitor struct {
    db          *state.DB
    interval    time.Duration
    batchSize   int
    mu          sync.RWMutex
    anvils      map[string]AnvilWicketConfig
    provider    ProviderFunc  // AI triage provider
    logger      *slog.Logger
    ghClient    GitHubClient  // interface for testability
}

// AnvilWicketConfig holds per-anvil Wicket settings.
type AnvilWicketConfig struct {
    AnvilName       string
    AnvilPath       string
    TrustedUsers    []string
    AutoDispatch    bool
    IssueLabels     []string
    Repos           []string   // "owner/repo" list; empty = anvil's own repo
    TriagePrompt    string
    ProcessedLabel  string
    NeedsHumanLabel string
    BeadCreatedLabel string
    TriggerLabel    string
}

// TriageDecision is the AI-determined outcome for an issue.
type TriageDecision struct {
    Action       TriageAction
    Reasoning    string   // AI's reasoning (logged, not posted publicly)
    BeadTitle    string   // populated when Action = CreateBead
    BeadBody     string   // populated when Action = CreateBead
    BeadType     string   // "bug", "feature", "task" (populated for CreateBead)
    BeadPriority int      // 0-3 priority (populated for CreateBead)
    BeadTags     []string // tags for the bead
    Response     string   // comment to post on the issue
    NeedsLabels  []string // additional labels to apply
}

type TriageAction string

const (
    ActionCreateBead    TriageAction = "create_bead"
    ActionAskClarify    TriageAction = "ask_clarification"
    ActionReject        TriageAction = "reject"
    ActionFlagHuman     TriageAction = "flag_human"
    ActionDuplicate     TriageAction = "duplicate"
    ActionAlreadyFixed  TriageAction = "already_fixed"
    ActionOutOfScope    TriageAction = "out_of_scope"
)
```

---

## 4. Issue Triage Logic and Decision Tree

### Phase 1: Pre-Filter (No AI Needed)

```
Issue arrives (new or updated)
  |
  +-- Already has wicket_processed_label? --> Skip
  |
  +-- Is a pull request? --> Skip
  |
  +-- Has wicket_trigger_label set AND issue lacks it? --> Skip
  |
  +-- Issue is closed? --> Skip
  |
  +-- Already tracked in wicket_issues table? --> Check for updates only
  |
  v
Determine user tier
  |
  +-- User in wicket_trusted_users? --> Full AI Triage (Phase 2a)
  |
  +-- User NOT in trusted list? --> Limited Triage (Phase 2b)
```

### Phase 2a: Full AI Triage (Trusted Users)

The AI agent receives the issue title, body, labels, and any comments, plus repo context (README, AGENTS.md, recent beads list for dedup). It returns a `TriageDecision`.

**Decision tree the AI is instructed to follow:**

```
Analyze issue
  |
  +-- Is this a duplicate of an existing open bead?
  |     --> ActionDuplicate: Comment linking to existing bead, close or label
  |
  +-- Is this already fixed on main?
  |     --> ActionAlreadyFixed: Comment explaining, suggest closing
  |
  +-- Is the issue clear and actionable?
  |     |
  |     +-- Yes: Has enough detail to implement?
  |     |     |
  |     |     +-- Yes --> ActionCreateBead
  |     |     |     - Generate bead title & description
  |     |     |     - Determine type (bug/feature/task) and priority
  |     |     |     - Comment on issue with bead details
  |     |     |     - Ask if user wants auto-dispatch (unless wicket_auto_dispatch=true)
  |     |     |
  |     |     +-- No --> ActionAskClarify
  |     |           - Comment with specific questions
  |     |           - List what information is missing
  |     |
  |     +-- No: Too vague or ambiguous
  |           --> ActionAskClarify
  |
  +-- Is this out of scope for the repo?
  |     --> ActionOutOfScope: Polite comment explaining
  |
  +-- Is this too complex/risky for autonomous fixing?
  |     --> ActionFlagHuman: Label for human review, comment explaining
  |
  +-- Is this a discussion/question rather than a bug/feature?
        --> ActionFlagHuman: Suggest using Discussions instead
```

### Phase 2b: Limited Triage (Non-Trusted Users)

For users not in the trusted list, the response is more conservative:

```
Analyze issue
  |
  +-- Is it clearly spam/off-topic?
  |     --> ActionReject: Flag for human, don't engage
  |
  +-- Otherwise:
        --> ActionFlagHuman
        - Post a polite, somewhat generic acknowledgment:
          "Thanks for opening this issue! A maintainer will review it shortly.
           In the meantime, if you can provide [reproduction steps / expected
           behavior / environment details], that would help us triage faster."
        - Apply needs-human label
```

### Phase 3: Post-Triage Follow-Up

When an issue that previously received `ActionAskClarify` gets a new comment from the author:

```
Author replied to clarification request
  |
  v
Re-run AI triage with full conversation context
  |
  +-- Now actionable? --> ActionCreateBead
  +-- Still unclear? --> ActionAskClarify (with updated questions)
  +-- Stale (no reply for 14 days)? --> Comment asking if still relevant
```

### Dispatch Confirmation Flow

When `wicket_auto_dispatch` is false and a bead is created:

```
Wicket comments on issue:
  "I've created bead **Forge-abc1** for this issue:
   - Title: <title>
   - Type: bug | Priority: 1
   - Description: <summary>

   Would you like me to dispatch this for automatic fixing?
   React with :rocket: or comment 'dispatch' to proceed.
   React with :label: or comment 'label <tag>' to add tags first."

User reacts/comments:
  --> "dispatch" or rocket reaction: bd update <id> --tag auto-dispatch
  --> "label X": bd update <id> --tag X, then ask about dispatch again
  --> No response: Bead stays queued for manual dispatch
```

---

## 5. GitHub API Interactions

### Using `gh` CLI (Preferred, Consistent with Existing VCS Layer)

Wicket uses `gh` CLI commands for GitHub interactions, consistent with how the rest of Forge operates. This avoids adding a Go GitHub API client dependency.

```go
// ListIssues fetches open issues for a repo, optionally filtered by label.
// Uses: gh issue list --repo owner/repo --state open --json number,title,body,author,labels,comments,createdAt,updatedAt
func (c *ghClient) ListIssues(ctx context.Context, repo string, opts ListOpts) ([]Issue, error)

// GetIssue fetches a single issue with full comment history.
// Uses: gh issue view <number> --repo owner/repo --json number,title,body,author,labels,comments
func (c *ghClient) GetIssue(ctx context.Context, repo string, number int) (*Issue, error)

// CommentOnIssue posts a comment.
// Uses: gh issue comment <number> --repo owner/repo --body "<text>"
func (c *ghClient) CommentOnIssue(ctx context.Context, repo string, number int, body string) error

// AddLabels adds labels to an issue.
// Uses: gh issue edit <number> --repo owner/repo --add-label "label1,label2"
func (c *ghClient) AddLabels(ctx context.Context, repo string, number int, labels []string) error

// RemoveLabel removes a label from an issue.
// Uses: gh issue edit <number> --repo owner/repo --remove-label "label"
func (c *ghClient) RemoveLabel(ctx context.Context, repo string, number int, label string) error

// CloseIssue closes an issue.
// Uses: gh issue close <number> --repo owner/repo --reason "not planned"|"completed"
func (c *ghClient) CloseIssue(ctx context.Context, repo string, number int, reason string) error

// ListReactions fetches reactions on an issue (for dispatch confirmation).
// Uses: gh api repos/{owner}/{repo}/issues/{number}/reactions
func (c *ghClient) ListReactions(ctx context.Context, repo string, number int) ([]Reaction, error)
```

### Rate Limiting Considerations

- GitHub API has 5,000 requests/hour for authenticated users
- `gh issue list` with `--json` is a single API call
- At 5-minute intervals with 10 repos, that is ~120 calls/hour for listing alone
- Add per-issue fetches for updated issues: budget ~500 calls/hour max
- Wicket should track `X-RateLimit-Remaining` and back off when low
- The `wicket_batch_size` setting caps per-cycle work

---

## 6. Bead Creation Workflow

### Step-by-Step

1. **AI generates bead metadata** from the issue:
   - Title (concise, actionable -- e.g., "Fix: crash when opening empty file" not "it crashes")
   - Description (structured: what, expected, actual, reproduction steps)
   - Type: bug / feature / task
   - Priority: 0 (critical) to 3 (low), inferred from issue urgency/impact
   - Tags: inferred from content (e.g., "ui", "api", "performance")

2. **Deduplication check** against existing beads:
   ```bash
   bd list --status=open,in_progress --limit 0 --json
   ```
   Compare title similarity and issue URL references to avoid duplicates.

3. **Create bead** via `bd create`:
   ```bash
   bd create \
     --title "Fix: <title>" \
     --description "<structured description>\n\nSource: <issue-url>" \
     --type bug \
     --priority 1 \
     --tag "wicket" \
     --tag "github-issue" \
     --json
   ```
   The `--tag wicket` tag identifies beads created by the Wicket for tracking.

4. **Link issue and bead** bidirectionally:
   - Comment on the GitHub issue with bead ID and details
   - Store the mapping in `wicket_issues` table (see section 8)

5. **Dispatch decision**:
   - If `wicket_auto_dispatch: true`, add the `auto-dispatch` tag immediately
   - If `wicket_auto_dispatch: false`, comment asking for confirmation (see section 4)
   - When user confirms, update the bead with dispatch tag

6. **Post-dispatch tracking**:
   - When the bead's PR is created (detected via Bellows events), comment on the issue with PR link
   - When the PR is merged, comment on the issue and close it

---

## 7. Response Templates

All responses are posted as GitHub issue comments. They should be professional but warm, and identify themselves as automated.

### Bead Created (Trusted User)

```markdown
Hey @{author}! I've reviewed this issue and created a work item for it.

**Bead:** `{bead_id}`
**Type:** {type} | **Priority:** P{priority}
**Title:** {bead_title}

<details>
<summary>Bead description</summary>

{bead_description}

</details>

{if auto_dispatch}
This has been queued for automatic fixing. I'll update this issue when a PR is ready.
{else}
**Next steps:** Would you like me to dispatch this for automatic fixing?
- Comment `dispatch` or react with :rocket: to proceed
- Comment `label <tag>` to add a tag before dispatching
- Or leave it for manual handling from the Forge queue
{end}

---
<sub>Triaged by [Forge Wicket](https://github.com/Robin831/Forge) | Bead {bead_id}</sub>
```

### Clarification Needed (Trusted User)

```markdown
Hey @{author}! Thanks for this issue. Before I can create a work item, I need a bit more detail:

{clarification_questions}

Once you've updated the issue or replied with more context, I'll re-evaluate and create a bead if everything checks out.

---
<sub>Triaged by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

### Duplicate Detected

```markdown
Hey @{author}! This looks like it may be a duplicate of an existing work item:

- **Bead:** `{existing_bead_id}` -- {existing_bead_title}
{if has_pr}- **PR:** {pr_url} (currently {pr_status}){end}

If this is a separate issue, please update the description to clarify how it differs, and I'll take another look.

---
<sub>Triaged by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

### Flagged for Human Review

```markdown
Hey @{author}! I've flagged this for a maintainer to review. {reason}

A human will take a look shortly.

---
<sub>Triaged by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

### Generic Response (Non-Trusted User)

```markdown
Thanks for opening this issue, @{author}! A maintainer will review it shortly.

In the meantime, if you can provide any of the following, it would help us triage faster:
- Steps to reproduce (if it's a bug)
- Expected vs. actual behavior
- Environment details (OS, version, etc.)

---
<sub>Triaged by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

### Rejected / Out of Scope

```markdown
Hey @{author}! Thanks for the suggestion. After reviewing, this falls outside the current scope of the project because:

{reason}

If you believe this should be reconsidered, feel free to open a discussion or ping a maintainer.

---
<sub>Triaged by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

### PR Created (Follow-Up)

```markdown
Update: A pull request has been created for this issue!

**PR:** {pr_url}
**Bead:** `{bead_id}`

I'll close this issue automatically when the PR is merged.

---
<sub>Updated by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

### PR Merged (Auto-Close)

```markdown
This has been fixed in {pr_url} and merged to `{base_branch}`. Closing this issue.

---
<sub>Closed by [Forge Wicket](https://github.com/Robin831/Forge)</sub>
```

---

## 8. State Tracking (in state.db)

### New Table: `wicket_issues`

```sql
CREATE TABLE IF NOT EXISTS wicket_issues (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo          TEXT    NOT NULL,           -- "owner/repo"
    issue_number  INTEGER NOT NULL,           -- GitHub issue number
    anvil_name    TEXT    NOT NULL,           -- which anvil this maps to
    author        TEXT    NOT NULL,           -- GitHub username
    is_trusted    BOOLEAN NOT NULL DEFAULT 0, -- trusted user?
    action        TEXT    NOT NULL,           -- triage action taken
    bead_id       TEXT,                       -- linked bead ID (nullable)
    pr_number     INTEGER,                    -- linked PR number (nullable)
    status        TEXT    NOT NULL DEFAULT 'open',  -- open, bead_created, dispatched, pr_created, merged, closed, stale
    reasoning     TEXT,                       -- AI reasoning (internal)
    created_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at    DATETIME NOT NULL DEFAULT (datetime('now')),
    last_polled   DATETIME,                  -- last time we checked for updates
    UNIQUE(repo, issue_number)
);

CREATE INDEX idx_wicket_issues_status ON wicket_issues(status);
CREATE INDEX idx_wicket_issues_repo ON wicket_issues(repo);
CREATE INDEX idx_wicket_issues_bead ON wicket_issues(bead_id);
```

### New Event Types

```go
const (
    EventWicketScanDone       EventType = "wicket_scan_done"
    EventWicketIssueTriage    EventType = "wicket_issue_triage"     // AI triage completed
    EventWicketBeadCreated    EventType = "wicket_bead_created"     // bead created from issue
    EventWicketClarification  EventType = "wicket_clarification"    // asked for clarification
    EventWicketRejected       EventType = "wicket_rejected"         // issue rejected
    EventWicketFlaggedHuman   EventType = "wicket_flagged_human"    // flagged for human
    EventWicketDispatchConfirm EventType = "wicket_dispatch_confirm" // user confirmed dispatch
    EventWicketPRLinked       EventType = "wicket_pr_linked"        // PR linked to issue
    EventWicketIssueClosed    EventType = "wicket_issue_closed"     // issue auto-closed after merge
    EventWicketError          EventType = "wicket_error"            // error during processing
)
```

### Integration with Bellows

Bellows already monitors PRs. When a PR created by a Wicket-originated bead is merged:

1. Bellows fires `EventPRMerged`
2. Wicket subscribes to merge events (or daemon bridges the event)
3. Wicket looks up the bead ID in `wicket_issues`
4. If found, posts the "PR Merged" comment and closes the GitHub issue

This can be implemented as a callback registered with Bellows or as an event listener in the daemon's main loop.

---

## 9. Edge Cases and Error Handling

### Issue Edge Cases

| Scenario | Handling |
|----------|----------|
| Issue edited after triage | Re-triage on next poll if `updated_at` changed since `last_polled` |
| Issue closed externally | Skip on next poll, mark as `closed` in `wicket_issues` |
| Issue reopened after close | Re-triage (reset status to `open`) |
| Issue with no body (title only) | AI decides: either ask clarification or infer from title |
| Very long issue body (>10k chars) | Truncate to first 10k chars for AI context, note truncation |
| Issue in non-English language | AI handles multilingual; responses in English (configurable later) |
| Multiple issues at once (burst) | `wicket_batch_size` caps per-cycle; FIFO by created_at |
| Issue references multiple repos | AI picks the most relevant anvil; flag for human if ambiguous |
| Bot-created issues (dependabot, etc.) | Skip issues from known bot accounts (configurable ignore list) |

### API / Infrastructure Edge Cases

| Scenario | Handling |
|----------|----------|
| `gh` CLI not authenticated | Log error, skip repo, surface in `forge doctor` |
| GitHub API rate limited | Back off exponentially, log `wicket_error` event |
| AI provider rate limited | Use provider fallback chain (same as Smith) |
| AI returns unparseable response | Retry once with stricter prompt; then flag for human |
| `bd create` fails | Log error, retry next cycle, don't comment on issue |
| Issue repo doesn't match any anvil | Skip (should not happen with config, but guard against it) |
| Duplicate bead detection false positive | Include issue URL in bead; AI checks URL match, not just title similarity |
| Network timeout during comment | Retry with backoff; track in `wicket_issues.status` |
| Config hot-reload changes trusted users | Existing triaged issues keep their action; new polls use updated list |
| Anvil removed from config | Stop monitoring; existing `wicket_issues` rows remain for history |

### Security Considerations

- Never include sensitive repo content in public GitHub comments
- AI triage prompt must not leak internal bead IDs in rejection messages (only in bead-created messages)
- Trusted user list is the primary access control; treat it as a security boundary
- Rate-limit comment posting to prevent abuse if an attacker creates many issues
- Sanitize AI-generated responses before posting (strip potential prompt injection artifacts)

---

## 10. Implementation Phases

### Phase 1: Core Scaffolding (MVP)

**Goal:** Wicket polls issues, triages trusted users, creates beads.

- [ ] Add config types: `WicketEnabled`, `WicketInterval`, `WicketTrustedUsers`, etc. to `config.go`
- [ ] Create `internal/wicket/` package with `Monitor`, `AnvilWicketConfig`, types
- [ ] Implement `ghClient` wrapper for `gh issue list/view/comment/edit`
- [ ] Create `wicket_issues` table in `state.db` (migration in `internal/state`)
- [ ] Add `EventWicket*` event types to `internal/state`
- [ ] Implement basic poll loop: list issues -> filter -> check `wicket_issues` for already-processed
- [ ] Implement AI triage for trusted users (ActionCreateBead, ActionAskClarify, ActionFlagHuman)
- [ ] Implement bead creation via `bd create`
- [ ] Post comments on issues with results
- [ ] Wire into daemon startup and hot-reload
- [ ] Add `forge wicket status` CLI command (show monitored repos, recent triage stats)
- [ ] Unit tests with mock `gh` output

**Estimated effort:** 3-4 days

### Phase 2: Non-Trusted Users + Labels

**Goal:** Handle non-trusted users; label management.

- [ ] Implement limited triage for non-trusted users (generic response + flag)
- [ ] Add label management (apply `forge-triaged`, `forge-needs-human`, `forge-bead-created`)
- [ ] Implement `wicket_trigger_label` (pull model: only process labeled issues)
- [ ] Implement `wicket_issue_labels` filter
- [ ] Bot account ignore list
- [ ] Add Hearth TUI integration: show Wicket activity in events column

**Estimated effort:** 2 days

### Phase 3: Dispatch Confirmation + Follow-Up

**Goal:** Interactive dispatch flow; issue lifecycle tracking.

- [ ] Implement dispatch confirmation (reaction/comment detection)
- [ ] Poll for user responses to clarification requests (re-triage)
- [ ] Implement issue-to-PR linking (listen for `EventPRCreated` matching Wicket beads)
- [ ] Auto-close issues when linked PR is merged
- [ ] Post PR-created and PR-merged comments
- [ ] Stale issue detection (no reply to clarification in 14 days)

**Estimated effort:** 2-3 days

### Phase 4: Advanced Triage + Polish

**Goal:** Smarter AI decisions; observability.

- [ ] Implement duplicate detection (compare against open beads via `bd list`)
- [ ] Implement "already fixed" detection (check recent merged PRs/closed beads)
- [ ] Implement "out of scope" detection with per-anvil triage prompt
- [ ] Add `forge wicket list` CLI command (show tracked issues, statuses)
- [ ] Add `forge wicket retriage <repo> <issue-number>` CLI command
- [ ] Add Wicket metrics to `forge status` output
- [ ] Rate limiting and backoff for GitHub API
- [ ] Integration tests against a test repo

**Estimated effort:** 2-3 days

### Phase 5: Multi-Repo + External Repos

**Goal:** Monitor issues on repos that aren't anvils themselves.

- [ ] Implement `wicket_repos` config (monitor external repos, create beads in anvil)
- [ ] Handle repo -> anvil mapping when creating beads
- [ ] Cross-repo deduplication
- [ ] Documentation in `docs/configuration.md`

**Estimated effort:** 1-2 days

---

## Appendix A: AI Triage Prompt (Draft)

```
You are a GitHub issue triage agent for the "{repo_name}" repository.
Your job is to analyze incoming issues and decide the best course of action.

Repository context:
{readme_summary}

Recent open beads (for dedup):
{open_beads_list}

{custom_triage_prompt}

Analyze the following GitHub issue and respond with a JSON decision:

Issue #{number}: {title}
Author: {author} (trusted: {is_trusted})
Labels: {labels}
Body:
{body}

Comments:
{comments}

Respond with EXACTLY this JSON structure:
{
  "action": "create_bead" | "ask_clarification" | "reject" | "flag_human" | "duplicate" | "already_fixed" | "out_of_scope",
  "reasoning": "<your internal reasoning, not shown to user>",
  "bead_title": "<if create_bead: concise actionable title>",
  "bead_body": "<if create_bead: structured description with what/expected/actual/steps>",
  "bead_type": "<if create_bead: bug|feature|task>",
  "bead_priority": <if create_bead: 0-3>,
  "bead_tags": [<if create_bead: relevant tags>],
  "response": "<comment to post on the issue>",
  "duplicate_of": "<if duplicate: existing bead ID>"
}

Guidelines:
- For bugs: priority 0 = data loss/security, 1 = broken core feature, 2 = broken non-core, 3 = cosmetic
- For features: priority 1 = highly requested, 2 = nice to have, 3 = stretch goal
- Ask clarification if: missing repro steps, ambiguous behavior, unclear scope
- Flag for human if: architecture changes, security concerns, breaking changes, multi-repo impact
- Reject if: spam, completely unrelated, already explicitly declined
```

## Appendix B: CLI Commands

```bash
forge wicket status                         # Show Wicket monitor status and stats
forge wicket list                           # List tracked issues and their status
forge wicket list --anvil <name>            # Filter by anvil
forge wicket retriage <repo> <issue-number> # Force re-triage of an issue
forge wicket pause                          # Temporarily pause Wicket (via IPC)
forge wicket resume                         # Resume Wicket
```

## Appendix C: IPC Extensions

New IPC commands for Wicket:

```json
{"command": "wicket_status"}
// Response: { "enabled": true, "repos": [...], "issues_tracked": 42, "beads_created": 12, "last_poll": "..." }

{"command": "wicket_list", "anvil": "Forge"}
// Response: { "issues": [{ "repo": "...", "number": 1, "action": "create_bead", "bead_id": "...", "status": "..." }] }

{"command": "wicket_retriage", "repo": "owner/repo", "issue": 42}
// Response: { "ok": true }
```

## Appendix D: Hearth TUI Panel

Wicket gets its own compact panel in the Hearth TUI, positioned **between Workers and Usage** in the right column (vertically). The Workers panel is currently taller than needed (never fills up), so we reclaim some of that vertical space for Wicket.

### Layout

```
┌─── Queue ──────────┐┌─── Workers ─────────┐
│                     ││ (shorter than today) │
│                     │├─── Wicket ───────────┤
│                     ││ forge     3 open  1⚠ │
│                     ││ heimdall  1 open  0⚠ │
│                     ││ hytte     0 open  0⚠ │
├─── Live Activity ──┤├─── Usage ────────────┤
│                     ││                      │
└─────────────────────┘└──────────────────────┘
```

### Content

The panel is intentionally minimal — just a per-anvil summary:

| Column | Description |
|--------|-------------|
| Anvil name | Left-aligned, same style as other panels |
| Open issues | Count of issues tracked by Wicket with status != closed/merged |
| ⚠ (needs human) | Count of issues flagged with `wicket_needs_human_label` |

The `⚠` count highlights in yellow/orange when > 0 to draw attention. Clicking/selecting an anvil row could open a detail view or jump to `forge wicket list --anvil <name>` output in the future.

### Sizing

- **Height:** 3-5 rows (header + one row per active anvil with Wicket enabled). Anvils with 0 open issues can be hidden to save space, or shown dimmed.
- **Workers panel** shrinks by the same amount. Workers panel currently allocates space for `max_total_smiths` rows but rarely fills — cap its minimum at e.g. 4 rows and give the rest to Wicket.

### Data Source

The panel subscribes to `wicket_status` IPC command or reads directly from `wicket_issues` table in state.db (same pattern as the queue panel reading from the poller).
