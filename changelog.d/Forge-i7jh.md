category: Added
- **Turn snapshot persistence** - Added a `forge_turn_snapshots` table and store API (`UpsertTurnSnapshot`, `GetLatestTurnSnapshot`, `GetTurnSnapshot`) so a mid-turn Beads-Forge AI turn — its status and accumulated text — survives a client reconnect or daemon restart. (Forge-i7jh)
