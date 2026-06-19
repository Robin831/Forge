category: Fixed
- **No duplicate Assay dispatch for an in-flight review** - The Bellows trigger gate now suppresses queueing a new Assay review when one is already pending/running for the PR, so a review that outlasts the debounce window is no longer re-queued for the same head (wasting an Assay run). (Forge-o81n)
