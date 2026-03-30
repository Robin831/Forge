category: Fixed
- **Pipeline PR base branch for dependency chains** - Regular blocked-by dependencies (task A blocks task B) no longer incorrectly route A's PR to `feature/B`. The epic branch lookup now only returns a feature branch for beads that are explicitly typed as `epic` or have an `epic-branch:` label; having "blocks"-type dependents alone is not sufficient. (Forge-t6y9)
