category: Added
- **Bead detail API now surfaces notes and comments** - `GET /api/bead/{id}` includes the bead's appended notes (from `bd show --json`) and the comment thread (from `bd comments --json`), unlocking the bead detail UI panels. Comment fetch failures are non-fatal and return an empty array. (Forge-fdz6)
