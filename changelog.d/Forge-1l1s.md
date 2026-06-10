category: Fixed
- **Hearth event filter searches the whole log** - The Events panel filter now runs a DB-backed search across the entire event log instead of only the ~100 events loaded in memory, so filtering for an older bead or event id matches correctly. Results are capped at 500 to protect the TUI render. (Forge-1l1s)
