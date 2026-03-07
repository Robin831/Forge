category: Fixed
- **Fix PR retry not re-triggering Bellows events** - Fixed an issue where retrying an exhausted PR (e.g., Rebase exhausted) would not trigger a new worker cycle immediately because the PR monitor's internal status cache was not cleared and no immediate poll was triggered. Added a refresh mechanism to the PR monitor. (Forge-u55)
