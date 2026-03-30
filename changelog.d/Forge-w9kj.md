category: Changed
- **Smelter startup-skip pattern** - The Smelter now skips its startup flush if a full cycle completed within the configured `smelter_interval`, preventing redundant PRs on daemon restarts. Logs `smelter_cycle_done` events after each flush cycle to track recency. For low-volume setups, `smelter_interval: 48h` or `72h` is a reasonable alternative to the default `8h`. (Forge-w9kj)
