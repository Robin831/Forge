category: Fixed
- **Bellows: PR title not stored in prs table** - Bellows now persists the PR title fetched from the VCS API on each status check, backfilling any empty titles from previous runs. The `CreatePR` path in the GitHub provider also now stores the title at insert time so ready-to-merge notifications show the actual PR title instead of "PR #N". (Forge-p9nu)

