category: Added
- **Event Bus fan-out on LogEvent** - Every logged event is now published to the daemon-owned in-process Bus after it is persisted, so subscribers receive events in real time. Publishing is non-blocking (drop-oldest) and carries the row's sequence/Last-Event-ID for re-sync. (Forge-r4fk)
