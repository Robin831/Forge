category: Fixed
- **Retry action on exhausted PRs now correctly resets counters** - Fixed a bug where the Hearth TUI "Retry" action did not pass the PR ID to the daemon, causing exhausted PRs (like those in Rebase Exhausted) to not be correctly reset. Also fixed the daemon's retry handler to properly call the database reset logic. (Forge-u55)
