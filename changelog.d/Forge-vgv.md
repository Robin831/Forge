category: Added
- **Depcheck dedup event logging** - When depcheck skips creating a bead due to deduplication (existing open, in-progress, or recently closed bead), a `depcheck_dedup` event is now logged to the event table so operators can see why an update wasn't created in Hearth's event log. (Forge-vgv)
