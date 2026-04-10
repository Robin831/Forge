category: Fixed
- **Schematic tolerates bd dep add non-zero exit** - When `bd dep add` exits non-zero but stdout confirms the dependency was added, treat it as success instead of marking the bead as needing clarification. (Forge-30a5)
