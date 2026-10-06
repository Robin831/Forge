category: Fixed
- **Dashboard 'Active workers' counts reviewing, paused and stalled workers** - The headline stat now uses the same live-worker predicate as the worker panel grid (every slot status, minus bellows PR monitors) instead of counting only pending/running, so a worker in Warden review, parked, or marked stalled no longer reads as finished on the summary. (Forge-ypsa)
