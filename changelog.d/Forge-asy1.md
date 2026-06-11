category: Changed
- **Assay runs outside the lifecycle worker cap** - Assay PR reviews are no longer gated by `max_lifecycle_workers`; they run unbounded (deduped per PR head) so reviews start immediately instead of queueing behind long-running Smith/Burnish/rebase sessions. (Forge-asy1)
