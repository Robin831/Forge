category: Changed
- **TUI async IPC correlation** - Hearth TUI now handles async "queued" IPC responses by subscribing to daemon completion events, correlating by request_id, and updating the status line with success or error messages. IPC read deadline reduced from 10s to 3s since only the fast initial ack needs to arrive. (Forge-pkkq)
