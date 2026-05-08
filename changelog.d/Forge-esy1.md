category: Added
- **Hearth 2.0 SSE activity stream and per-worker log tail** - The web dashboard now subscribes to `/api/activity/stream` for real-time event updates and exposes per-worker log access via `/api/worker/{id}/log?tail=N` and `/api/worker/{id}/stream` (SSE), so the UI no longer polls for events and worker logs can be tailed live or replayed for completed workers. (Forge-esy1)
