category: Changed
- **Hearth event feed rides the IPC subscribe stream** - When the event bus is enabled, the daemon forwards logged events over IPC and the Hearth TUI renders them live instead of ticker-polling the events table (status/queue polls still tick). IPC Broadcast now targets only subscribed connections so streamed events never race a command's response. (Forge-4ufm)
