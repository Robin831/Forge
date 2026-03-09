category: Fixed
- **PRs no longer flash in Ready to Merge while Copilot review is pending** - New PRs now default to has_pending_reviews=1 (pending) and only appear in Ready to Merge after bellows confirms no reviews are outstanding. The merge handler also now checks for pending review requests in its live readiness gate. (Forge-6sc)
