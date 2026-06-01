category: Added
- **Assay re-review command** - New `assay_rerun {anvil, pr}` IPC command and `forge assay run --pr N --anvil A` CLI subcommand trigger a fresh Assay AI review over a PR's current head, bypassing the Bellows head-SHA debounce. The web "Re-run" button routes through the same command. (Forge-rem1)
- **Per-pass Assay doctor check** - `forge doctor` now verifies the `claude` (or configured provider) binary is available for every Assay review pass, reported per-pass. (Forge-rem1)
