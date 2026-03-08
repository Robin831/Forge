category: Added
- **Depcheck deduplication before creating update beads** - The dependency checker now checks existing open, in-progress, and recently-closed beads (last 7 days) plus open PRs before creating new dependency update beads. This prevents duplicate beads from accumulating across scan cycles. Dependency bead titles now use the `Deps(Go):` prefix convention. (Forge-2cs)
