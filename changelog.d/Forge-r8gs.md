category: Added
- **Wicket `wicket_repos` config + repo resolution** - Added `RepoResolver` type in `internal/wicket/repos.go` that resolves the GitHub repository list for each anvil: uses explicit `wicket_repos` when configured, otherwise derives the repo from `git remote get-url origin`. The Monitor now maintains a `repo→anvil` mapping updated on each scan cycle so downstream dispatch and clarification code can look up anvil ownership without re-running git. (Forge-r8gs)

