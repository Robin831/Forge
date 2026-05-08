category: Added
- **Hearth 2.0 destructive admin actions** - Wire IPC kill/refresh/queue commands and bd subprocess calls into the web UI: kill worker, retry/dispatch/clarify/unclarify/stop bead, close bead, add/remove labels, and append notes. Each action goes through a confirmation modal and surfaces errors via toast. (Forge-cnj8)
