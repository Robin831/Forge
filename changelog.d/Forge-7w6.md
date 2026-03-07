category: Fixed
- **Merge action errors now logged as daemon events and shown prominently in TUI** - When `gh pr merge` fails, a `pr_merge_failed` event is logged to the daemon event log (previously only `pr_merge_requested` was recorded). The TUI now shows merge errors in red in the status bar for 10 seconds (up from 5) and adds them to the Events panel for persistent visibility. (Forge-7w6)
