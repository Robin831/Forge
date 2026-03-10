category: Added
- **Daemon health indicator in Hearth TUI** - The header now shows a live connection indicator (● Connected / ○ Disconnected) with last poll time, so you can tell at a glance if the daemon is alive. (Forge-dl56)
- **`forge status --brief` flag** - One-line output suitable for shell prompts and status bars (e.g. `⚒ 2 smiths | 5 queued | 3 PRs | $1.23 | polled 30s ago`). (Forge-dl56)
- **Live last-poll and queue-size in `forge status`** - The daemon now tracks actual last poll time and queue size instead of showing "n/a" and 0. (Forge-dl56)
