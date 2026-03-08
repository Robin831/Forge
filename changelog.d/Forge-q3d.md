category: Added
- **Crucible: detect parent-child from blocks/blocked_by dependency graph** - Children that block an epic-type bead are now automatically routed through the epic's feature branch without requiring the parent field to be set. This resolves the chicken-and-egg problem where beads with parent set were hidden from `bd ready`. (Forge-q3d)
