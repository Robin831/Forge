category: Added
- **Worktree cleanup diagnostic logging** - Added probeNodeModules instrumentation around all git subprocess and os.RemoveAll calls in worktree cleanup paths, and bumped junction unlink log level from Debug to Info, to diagnose node_modules wipe issues. (Forge-bk0q)
