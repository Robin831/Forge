category: Fixed
- **PR creation failures now surface in Needs Attention panel** - When CreatePR fails in finalizePipeline, a `pr_creation_failed` DB event is logged and the bead is marked `needs_human` so it appears in `forge history events` and the Hearth TUI. Duplicate PR errors (re-runs) are handled gracefully with a warning instead of failing. (Forge-yvu6)
