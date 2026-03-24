category: Fixed
- **CLI update-deps worktree isolation** - The `forge update-deps --create-pr` command now creates an isolated git worktree for each anvil (matching the Hearth/Ledger overlay pattern), so dep commits land on the batch-update branch instead of main, multi-anvil runs no longer reset each other's branch, and the main anvil directory stays on main throughout. (Forge-n3m4)

