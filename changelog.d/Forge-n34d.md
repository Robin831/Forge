category: Fixed
- **Schematic-phase workers now count against the dispatch cap** - A dispatched worker in the schematic pre-analysis phase no longer drops out of the `max_total_smiths`/`max_smiths` count, closing a window where a second Smith could be dispatched concurrently. (Forge-n34d)
