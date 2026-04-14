category: Fixed
- **Temper blocks npm ci when node_modules is a junction** - Detect when node_modules is a symlink/junction (from worktree linking) and skip destructive install commands like `npm ci` that would wipe the shared main checkout's dependencies. The step is skipped with a clear explanation instead of failing with EPERM. (Forge-bdqz)
