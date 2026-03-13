category: Added
- `forge queue stop <id> --anvil <name>` command to fully stop a bead: kills the running worker, sets clarification_needed (preventing re-dispatch), and releases the bead back to open. Use `forge queue unclarify` to resume.
- Hearth TUI: press `S` on a worker in the Workers panel to stop its bead entirely.
- Hearth TUI: "Stop" action in the Queue panel action menu (Enter on a queue item).
