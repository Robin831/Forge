category: Fixed
- **Skip node_modules junction for dependency-update beads** - Depcheck beads now get a `deps-update` label, and worktree setup skips the node_modules junction for these beads so npm install writes to a fresh local directory instead of corrupting the main checkout. (Forge-0e0v)
