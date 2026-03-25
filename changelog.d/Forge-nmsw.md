category: Fixed
- **Smelter skips redundant startup scan** - Vulncheck no longer re-scans on every daemon restart; if a scan already completed today (recorded in state.db), the startup scan is skipped and the next run follows the normal interval schedule. (Forge-nmsw)
