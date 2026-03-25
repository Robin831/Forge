category: Fixed
- **Wicket pending issues no longer stuck after bead creation failure** - `shouldSkip` now treats a `pending` issue with no `bead_id` as retryable instead of permanently skipping it. The triage loop also handles the retry case when the pending row already exists in state.db, allowing the next scan cycle to reattempt bead creation without manual DB intervention. (Forge-bzii)

