category: Fixed
- **Orphaned worker logs after worktree cleanup** - Repoint each worker's `log_path` to the preserved copy under `~/.forge/logs/<bead>/` when a pipeline's worktree is removed, and backfill pre-existing dangling rows on daemon startup, so historical worker logs are viewable again in the web UI. (Forge-59h1)
