category: Added
- **Two-tier polling for faster bead discovery** - The poller now uses a fast label-filtered path every poll interval and a slow unfiltered path on a separate cadence (configurable via `crucible_poll_interval`, default 3m) to rebuild the Crucible parent-child graph. This reduces no-op poll cycle time by ~3.6x while preserving Crucible candidate detection. (Forge-fbyh)
