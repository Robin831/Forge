category: Added
- **Smelter staleness pass (Pass 2)** - After the consolidation pass, the Smelter now sweeps the active warden rules and moves entries older than `settings.warden.archive_after_days` (default 180) into `.forge/warden-rules.archive.yaml` with `archive_reason: stale`. The commit subject and `smelter_flushed` events surface how many rules aged out per anvil. (Forge-ep74)
