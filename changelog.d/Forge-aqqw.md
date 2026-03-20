category: Changed
- **Warden learn routes to pending table when smelter enabled** - When `smelter_enabled` is true, learned warden rules from both auto-learn (Copilot comments) and CI fix learning are inserted into the `pending_warden_rules` table instead of creating immediate PRs or saving directly to the rules file. This allows the Smelter to batch-process rule additions. (Forge-aqqw)
