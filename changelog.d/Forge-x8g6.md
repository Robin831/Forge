category: Fixed
- **Bellows PR merge no longer fails on active worktree branch** - Pass `--delete-branch=false` to `gh pr merge` so local branch cleanup is left to the worktree teardown, preventing a false failure when the branch is still in use. (Forge-x8g6)
