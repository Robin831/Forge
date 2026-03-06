category: Fixed

- **Bellows: dispatch second-wave review comments after fix cycle completes** - After a review fix worker finishes and re-requests review, `NeedsFix` is now cleared so the next `EventReviewChanges` from the re-review dispatches a new fix cycle instead of being silently dropped. (Forge-o4m)
- **Bellows: stale needs_fix PRs no longer stick across daemon restarts** - `Load()` resets `needs_fix` DB status back to `open` on startup, and `NotifyReviewFixCompleted` persists `PROpen` to the DB so the cleared state survives a restart. (Forge-o4m)
