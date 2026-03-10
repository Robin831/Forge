category: Fixed
- **Crucible child failures now correctly prevent standalone re-dispatch** - Failed crucible children use the "circuit breaker:" LastError prefix and the dispatch filter now checks all needs_human=1 rows, preventing re-dispatch outside crucible context. Also preserves existing retry counters and logs UpsertRetry errors. (Forge-roki)
