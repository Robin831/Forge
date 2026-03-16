category: Changed
- **Platform-aware `gh` check in `forge doctor`** - The GitHub CLI check now only runs when at least one anvil uses the GitHub platform. Non-GitHub setups (GitLab, Gitea, Bitbucket, Azure DevOps) no longer report a false failure for missing `gh`. (Forge-khg5)
