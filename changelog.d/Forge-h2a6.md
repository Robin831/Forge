category: Fixed
- **Bellows re-emits EventPRConflicting when PR stays conflicting across a failed rebase cycle** - Mirror the CI still-failing and review still-unresolved retry branches on the rebase path so a transient `git fetch` failure (or any aborted rebase) no longer permanently strands a CONFLICTING PR. (Forge-h2a6)
