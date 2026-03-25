category: Fixed
- **Ledger close bead fails with exit status 1** - Added `--json` flag to all `bd close` calls in the Ledger so they run non-interactively (matching how the daemon closes beads). Also improved `bdExec` error reporting to include stdout content when stderr is empty, surfacing the real failure reason. (Forge-cspl)
