category: Fixed
- **Close beads for PRs merged during daemon downtime** - On startup, the daemon now runs a reconciliation pass that detects merged PRs in state.db whose beads are still open, and closes them. This prevents beads from staying in_progress indefinitely when a PR merges during a restart window. (Forge-q0qp)
