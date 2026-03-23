category: Added
- **Wicket daemon wiring and CLI status command** - Wire the Wicket issue triage monitor into the daemon lifecycle (startup, hot-reload, shutdown) and add `forge wicket status` command that displays enabled state, monitored repos, issue counts by triage state, and poll interval via IPC. (Forge-qcae)
