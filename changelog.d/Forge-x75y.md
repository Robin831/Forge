category: Fixed
- **Schematic auto-closes parent bead after decomposition** - After decomposing a parent bead into sub-beads, the parent is now closed with `--force` instead of being left open. This prevents Forge from re-dispatching the parent once all sub-beads complete. (Forge-x75y)

