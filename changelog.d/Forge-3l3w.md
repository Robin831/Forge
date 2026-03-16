fix: allow all lifecycle actions on non-bead PRs (warden-learn)

The BeadID guard added for Forge-6sed was too broad — it blocked both
Bellows-triggered and user-triggered burnish/quench/rebase on PRs with no
associated bead (e.g. warden-learn PRs).

Added `IsManual bool` to `lifecycle.ActionRequest` and set it in IPC
pr_action handlers. The guard now only bails out if there is neither a
BeadID nor a PR number — i.e. no key can be derived at all. Both automatic
(Bellows) and manual (Hearth) actions on non-bead PRs proceed using
`pr-{N}` as the worktree and in-flight lock key, which avoids the
`.workers/` directory corruption the original guard was protecting against.
