category: Added
- **Force-run bead independently** - Added `--force` flag to `forge queue run` and "Run independently" action to the Hearth queue menu. Force-run fetches the bead via `bd show` (bypassing `bd ready`), skips crucible detection and parent/blocker checks, and dispatches it straight through the pipeline as a standalone bead. Requires `--anvil` flag. (Forge-qxec)
