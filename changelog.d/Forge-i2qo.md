category: Changed
- **First-anvil-wins dedup at DB layer** - Wicket now explicitly detects UNIQUE constraint violations on `wicket_issues` INSERT and logs a clear "issue already tracked by another anvil, skipping" warning, implementing first-anvil-wins semantics to prevent double-processing across multiple anvils. (Forge-i2qo)
