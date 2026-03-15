category: Fixed
- **Orphan recovery no longer races with post-merge bead close** - Extended HasOpenPRForBead to also protect beads with recently-merged PRs (within a 10-minute grace window), preventing orphan recovery from falsely resetting beads to open before the async bd close completes. (Forge-hp1h)
