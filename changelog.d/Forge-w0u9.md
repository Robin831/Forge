category: Added
- **Periodic refresh with cursor state preservation** - The TUI now refreshes every 5 seconds with a concurrent-fetch guard (`refreshing` flag) to prevent overlapping refresh cycles, and restores the queue cursor to the previously focused bead by ID after each refresh so navigation is not disrupted by list updates. (Forge-w0u9)
