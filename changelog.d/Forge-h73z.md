category: Fixed
- Pipeline retry now resets the worktree branch to the base ref (origin/main) instead of reusing commits from a failed run, preventing cascading junk commits and wasted API spend on hopeless retries.
