category: Fixed
- **Parallelize auto_dispatch tag propagation** - Child bead tag propagation in applyDecomposedOutcome now runs concurrently instead of sequentially, reducing wall-clock time from ~100s to ~25s for a 4-child decompose. (Forge-n50j)
