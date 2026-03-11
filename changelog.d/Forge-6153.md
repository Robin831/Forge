category: Fixed
- **Preserve smith logs after worktree cleanup** - Smith log files from `.forge-logs/` are now copied to `~/.forge/logs/<bead-id>/` before the worktree is removed, making post-mortem debugging possible after pipeline completion or failure. The worker's `log_path` in the state DB is updated to point to the persistent location. (Forge-6153)
