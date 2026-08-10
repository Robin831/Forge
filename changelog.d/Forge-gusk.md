category: Added
- **Assay reruns by PR number** - `assay_rerun` now accepts either the state.db PR row id (`pr`, unchanged) or the GitHub PR number scoped by anvil (`pr_number`), resolved through one shared helper with `pr_action`'s row lookup; supplying both or neither is refused with a clear message instead of guessed at. (Forge-gusk)
- **`forge assay rerun <pr> --anvil <name>`** - New CLI verb that re-runs Assay over a PR's current head addressed by its GitHub PR number, alongside the existing `forge assay run --pr <id>` which addresses the state.db row id. (Forge-gusk)
