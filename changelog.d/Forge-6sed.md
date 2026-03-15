category: Fixed
- **Rebase/cifix/burnish skip non-bead PRs** - Lifecycle workers (rebase, CI-fix, review-fix) no longer fire for warden-learn PRs that have no associated bead ID. Previously, these PRs would trigger a rebase worker that ran in the `.workers/` directory itself (due to empty bead ID collapsing the worktree path), causing Smith to operate in the wrong directory. (Forge-6sed)
