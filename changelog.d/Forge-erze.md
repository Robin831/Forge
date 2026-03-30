category: Changed
- **Lifecycle worker stale detection** - Lifecycle workers (CI fix, review fix, rebase) are now registered with a per-worker stale timeout (half of `smith_timeout`) so they can be detected as stalled even though they are excluded from the global background-phase stale check. Adds `stale_timeout` column to the workers table and `StaleTimeout` field on `state.Worker`. (Forge-erze)

