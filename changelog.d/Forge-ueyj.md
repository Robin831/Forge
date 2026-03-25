category: Fixed
- **Auto-create PR for orphaned branch on NO_CHANGES_NEEDED** - When Smith reports NO_CHANGES_NEEDED but a forge branch with commits ahead of main has no open PR, the daemon now automatically creates the PR instead of flagging needs_human. Only if PR creation itself fails does the bead escalate to needs_human, eliminating the most common "last mile" stuck scenario. (Forge-ueyj)
