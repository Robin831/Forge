category: Changed
- **PR-findings stream rides the event bus** - The PR findings SSE stream now re-emits on a dedicated findings-changed bus notification (published when an Assay pass records its run) instead of re-reading the snapshot every 2s, so new findings reach the PR detail panel in real time. Falls back to 2s polling when the bus is disabled or `sse_poll_fallback` is set. (Forge-cug5)
