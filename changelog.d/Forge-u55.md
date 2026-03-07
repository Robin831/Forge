category: Fixed
- **Retry action for exhausted PRs** - Fixed a race condition in the Bellows monitor that could cause manual retries of exhausted PRs (e.g., Rebase exhausted) to be ignored by the background monitoring loop. (Forge-u55)
