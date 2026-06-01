category: Added
- **Assay review trigger gate** - Bellows now emits a `pr_review_needed` event and dispatches an Assay review action when a managed, open, non-draft PR's head differs from the last reviewed commit, CI has settled, the per-(anvil, PR) debounce window has elapsed, and the daily Assay cost cap is not exceeded. (Forge-wxxv)
