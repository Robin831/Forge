category: Fixed
- **Worktree Remove unlinks junctions before removal** - Manager.Remove now calls unlinkReparsePoints before git worktree remove, preventing node_modules junction targets from being destroyed during worker cleanup. (Forge-9145)
