category: Added
- **Bead detail dependency lists** - `/api/bead/{id}` now returns `blocks` and `blocked_by` arrays sourced from `bd show <id> --json`, and a new `/api/bead/{id}/deps?depth=N` endpoint walks the dep graph (capped at depth 3) for the Hearth 2.0 modal/graph views. (Forge-98z7)
