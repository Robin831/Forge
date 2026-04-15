category: Fixed
- **Downgrade node_modules diagnostic probes to debug level** - All worktree cleanup paths that could wipe main node_modules via junction/symlink traversal are now guarded; diagnostic probes demoted from Info to Debug to reduce log noise. (Forge-orzd)
