category: Fixed
- **TUI update panel now applies deps via dedicated branch and PR** - The Hearth and Ledger U panel now creates a dedicated `deps/batch-update-<date>` branch, applies updates there, generates a changelog fragment, and opens a GitHub PR. Previously, updates were committed directly to the current branch (typically main). (Forge-9f1g)
