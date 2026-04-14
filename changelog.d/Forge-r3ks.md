category: Changed
- **Async IPC command handling** - Daemon handlers that shell out to bd or gh (tag_bead, close_bead, stop_bead, retry_bead, pr_action close/merge, append_notes) now return a queued response immediately and execute the subprocess in a background goroutine, preventing IPC blocking during slow remote operations. (Forge-r3ks)
