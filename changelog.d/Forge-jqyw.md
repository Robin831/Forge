category: Added
- **Auto-link node_modules in worktrees** - After creating a git worktree, Forge now automatically symlinks (or creates junctions on Windows) node_modules directories from the main checkout into the worktree. This eliminates the need for npm ci during temper for Node.js projects. (Forge-jqyw)
