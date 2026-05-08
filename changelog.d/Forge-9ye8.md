category: Added
- **PR data sources for the /prs tab** - Hearth 2.0's `/prs` tab now lists Forge-managed PRs, externally-authored PRs, and PRs merged in the last 7 days. Sections are populated by a new `GET /api/prs/all` endpoint backed by state.db, with a 60-second client-side cache and manual refresh. (Forge-9ye8)
