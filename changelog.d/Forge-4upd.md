category: Fixed
- **Depupdate worktree isolation** - Dependency update runs (Hearth/Ledger update panel) now execute in an isolated git worktree instead of checking out a branch directly in the main anvil directory, keeping the anvil on `main` and preventing conflicts with concurrent Smith/Bellows operations. (Forge-4upd)
