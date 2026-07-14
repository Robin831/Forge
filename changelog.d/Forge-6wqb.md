category: Added
- **In-process event bus foundation** - Add a concurrency-safe `state.Bus` with bounded per-subscriber buffers, drop-oldest overflow handling, and gap-marker sentinels so slow clients learn to re-sync. Carries a `BusEvent` payload reusing the events row shape plus a Last-Event-ID sequence. (Forge-6wqb)
