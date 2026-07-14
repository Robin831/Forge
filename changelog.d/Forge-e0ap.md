category: Added
- **Partial turn read-back on reconnect** - When a Beads-Forge chat client reconnects to a turn whose live state was lost (daemon restart, GC expiry, or retention-cap eviction), the SSE stream now replays the persisted mid-turn snapshot's accumulated text as a text_delta before the graceful turn_expired event, so partial output is recovered instead of an empty bubble. (Forge-e0ap)
