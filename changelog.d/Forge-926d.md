category: Fixed
- **Smith timeout and watchdog suspended while a bead is paused** - The smith timeout deadline is now advanced by the wall-clock time a bead spends parked, so a pause no longer expires the smith budget; the stale-worker watchdog skips paused workers; and a parked worker's git worktree is retained across daemon restart instead of being cleaned up as abandoned. (Forge-926d)
