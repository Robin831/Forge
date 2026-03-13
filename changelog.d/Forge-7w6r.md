### Added
- Hearth: dedicated PR panel overlay (press `p`) listing all open PRs with status indicators (CI, conflicts, reviews, approval) and action menu (Open in browser, Fix comments, Resolve conflicts, Close PR)
- New `pr_action` IPC command for triggering reviewfix, rebase, close, and open-in-browser on any open PR
- New `OpenPRsWithDetail()` database query for PR panel data with title resolution
