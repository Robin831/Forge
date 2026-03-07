category: Fixed
- **Fix PR retry not re-triggering Bellows events** - Fixed an issue where retrying an exhausted PR (e.g., Rebase exhausted) would not trigger a new worker cycle because the PR monitor's internal status cache was not cleared. (Forge-u55)
