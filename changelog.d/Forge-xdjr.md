category: Fixed
- **update-deps scan worktree isolation** - Create a temporary git worktree for the scan phase (including `--dry-run`) so that `npm ci` runs in an isolated directory rather than the main repo. This avoids EPERM errors on Windows when `.node` binaries in the main repo's `node_modules` are locked by editors or antivirus. Falls back to scanning the main directory if worktree creation fails. (Forge-xdjr)

