category: Fixed
- **Bellows now closes deferred beads when their PR merges** - Beads with dependents had their close deferred until PR merge, but the lifecycle handler tried to create a worktree first (which could fail), preventing the bead from ever being closed. The close action now runs directly without a worktree. (Forge-lce6)
