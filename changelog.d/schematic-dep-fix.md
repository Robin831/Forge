category: Fixed
- **Schematic chain transfer uses live dependency data** - The chain-aware decomposition now re-fetches the parent bead's dependencies via `bd show --json` before transferring, fixing a bug where the `blocks` and `depends_on` fields were empty due to JSON field name mismatch between bd output and the internal struct. (Forge-29ub)
