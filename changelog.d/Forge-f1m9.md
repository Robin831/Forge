category: Fixed
- **Temper path filtering resolved refs from the wrong repository** - `resolveTemperBaseRef` and `temper.ChangedFilesFromGit` now run with `GIT_DIR`/`GIT_WORK_TREE` stripped, so a daemon started from inside a git worktree can no longer have its own repository answer for the anvil's when computing the base ref and changed-file list. (Forge-f1m9)
