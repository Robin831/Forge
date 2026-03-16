fix: allow manual burnish/quench/rebase on non-bead PRs (warden-learn)

The BeadID guard added for Forge-6sed was too broad — it blocked user-triggered
burnish, quench, and rebase actions from Hearth on PRs with no associated bead
(e.g. warden-learn PRs). Only automatic Bellows-triggered actions should be
skipped for non-bead PRs.

Added `IsManual bool` to `lifecycle.ActionRequest`. IPC pr_action handlers now
set `IsManual: true` for burnish/quench/rebase. The guard skips only automatic
(non-manual) actions with an empty BeadID. For manual actions on non-bead PRs,
`pr-{N}` is used as the worktree and in-flight lock key so the path is valid.
