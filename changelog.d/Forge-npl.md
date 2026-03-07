category: Added

- **Hearth: Ready to Merge panel** - New panel in the TUI dashboard showing PRs that are ready to merge (CI passing, approved, no conflicts, no unresolved threads). Press Enter to open a merge action menu that calls `gh pr merge` via the daemon. Merge strategy is configurable via `settings.merge_strategy` in forge.yaml (default: squash). (Forge-npl)
