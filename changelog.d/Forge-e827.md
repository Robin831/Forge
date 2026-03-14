category: Added
- **Per-anvil auto-merge for ready PRs** - Added `auto_merge` config option for anvils that automatically merges PRs when they reach the ready-to-merge state (CI passing, no conflicts, no unresolved threads, no pending reviews). External PRs are never auto-merged. The Ready to Merge panel in Hearth shows an `[auto]` tag for PRs that will be auto-merged. (Forge-e827)
