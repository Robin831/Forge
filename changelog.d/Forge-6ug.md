category: Fixed
- **PRs no longer briefly appear in Ready to Merge** - Set has_pending_reviews immediately after PR creation by calling CheckStatus, instead of waiting for the next bellows poll cycle. (Forge-6ug)
