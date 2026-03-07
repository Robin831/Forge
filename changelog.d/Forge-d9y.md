category: Fixed
- **Vulnerability scan no longer crashes when govulncheck is missing** - The scanner now checks for `govulncheck` in PATH before attempting scans, logging a clear warning instead of failing with a cryptic `exec:` error. (Forge-d9y)
