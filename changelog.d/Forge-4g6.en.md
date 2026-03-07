category: Added
- **CI check: enforce changelog fragment on PRs** - Smith prompt now instructs workers to create a `changelog.d/<bead-id>.en.md` fragment for user-visible changes. A GitHub Actions workflow job also fails PRs that modify user-visible code without a fragment (opt out with `[no-changelog]` in the PR title). (Forge-4g6)
