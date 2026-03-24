category: Added
- **Wicket metrics in forge status** - `forge status` now includes a Wicket Issues section showing total tracked issues, beads created, last scan time, and breakdowns by lifecycle state and triage action. Metrics are sourced from the wicket_issues table via a new `GetWicketMetrics()` state DB method and surfaced through the IPC status payload. (Forge-e4vm)
