category: Fixed
- **Retry action for exhausted PRs** - Fixed a race condition in the Bellows monitor that could cause manual retries of exhausted PRs (e.g., Rebase exhausted) to be ignored. Also ensured that merge conflicts correctly transition PR status to "needs fix" to improve visibility in the dashboard. (Forge-u55)
