category: Added
- **Warden rule archive store** - Persist superseded or stale review rules to `.forge/warden-rules.archive.yaml` with reason and timestamps, plus `settings.warden.archive_after_days` (default 180) and `settings.warden.dedup_threshold` (default 0.6) config keys to drive future archive/dedup passes. (Forge-7pu1)
