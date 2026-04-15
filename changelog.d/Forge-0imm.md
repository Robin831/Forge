category: Fixed
- **Protect node_modules junction from git clean in worktree reuse and deny-pattern reset** - Unlink junctions/symlinks before git checkout --force, git clean -fd, and git reset --hard so git cannot traverse into the main checkout's node_modules and destroy its contents. Also exclude node_modules from git clean and re-link after pipeline deny-pattern resets. (Forge-0imm)
