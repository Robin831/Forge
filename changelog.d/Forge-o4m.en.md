category: Fixed
- **Bellows: dispatch second-wave review comments after fix cycle completes** - After a review fix worker finishes and re-requests review, `NeedsFix` is now cleared so the next `EventReviewChanges` from the re-review dispatches a new fix cycle instead of being silently dropped. (Forge-o4m)
