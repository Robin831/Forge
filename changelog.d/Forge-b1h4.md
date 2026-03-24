category: Fixed
- **Adventurer test timeout** - Apply executor timeout to click/fill/assert element lookups and add t.Parallel() so tests run concurrently; prevents 300s suite timeout caused by rod's default 30s per-element wait. (Forge-b1h4)

