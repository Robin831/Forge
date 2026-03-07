category: Added

- **Scheduled Go module update checker** - New `depcheck` monitor periodically runs `go list -m -u all` on Go anvils to detect outdated dependencies. Patch/minor updates create auto-dispatch beads; major version bumps create separate "needs attention" beads. Configurable via `depcheck_interval` (default: weekly) and `depcheck_timeout` settings. (Forge-t1d)
