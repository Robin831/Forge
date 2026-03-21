category: Fixed
- **Smelter force-push on fresh worktree** - Fetch the batch branch from origin before pushing so `--force-with-lease` has a remote-tracking ref to compare against. Without this fetch, a freshly created worktree had no tracking ref and the push was rejected when the branch already existed on origin from a prior smelter run. (Forge-oc7l)
