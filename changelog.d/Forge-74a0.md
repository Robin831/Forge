category: Fixed
- **Prevent re-dispatch of decomposed parent beads** - When a decomposed parent bead has dependents it stays open until they complete, then becomes unblocked and re-dispatched. The daemon now tags the parent with `forge-decomposed` so schematic detects it, returns `ActionAlreadyDecomposed`, and the pipeline closes the bead without spawning a new Smith session. (Forge-74a0)
