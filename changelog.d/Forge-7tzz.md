category: Added
- **`forge backfill-anvils` admin command** - Heuristically populates empty `anvil` on legacy `forge_sessions` rows by case-insensitive substring matching of the session title against registered anvil names. Supports `--dry-run`; skips rows with zero or multiple matches and prints a summary. (Forge-7tzz)
