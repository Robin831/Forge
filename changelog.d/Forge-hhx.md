category: Fixed

- **Stale worker detection excludes bellows/lifecycle workers** - Bellows, cifix, and reviewfix workers are no longer flagged as stalled since they are long-running background processes that naturally have infrequent log activity (Forge-hhx)
