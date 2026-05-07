category: Fixed
- **Stale `core.worktree` self-heal in worktree manager** - Worktree creation and removal now detect and unset a stale `core.worktree` setting on the main repo when it points to a removed-or-empty worker path. The stale value caused `git status --porcelain` to fail with exit 128, which in turn broke Go's VCS stamping during `go build` from any worker. (Forge-d6j5)
