category: Fixed
- **Distinguish warden hard-reject from request-changes in event log** - Added a new `warden_hard_reject` event type for terminal warden rejections, separate from `warden_reject` which now exclusively represents request-changes verdicts. This makes it clear in the event log why a bead stopped early instead of iterating up to `max_pipeline_iterations`. (Forge-erdg)
