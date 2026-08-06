category: Added
- **Preview idle countdown and resource note in the dashboard** - GET /api/previews now forwards the preview manager's `idle_remaining_seconds` and `resource_note` (the same fields `forge preview list` reads over IPC), and the Hearth preview panel and previews page render the countdown as a live "idles in …" value plus the note as secondary text. (Forge-7n47)
