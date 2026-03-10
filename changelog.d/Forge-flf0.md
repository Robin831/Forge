category: Fixed
- **Crucible child failure no longer causes orphan recovery loop** - When a crucible child fails, the child bead is now reset to open and marked needs_human so orphan recovery won't pick it up and dispatch it as a standalone bead outside crucible context. (Forge-flf0)
