category: Changed
- **Skip Schematic pre-analysis when primary provider is Copilot** - Copilot charges per-request rather than per-token, so Schematic pre-analysis would consume an extra premium request. The phase is now skipped automatically when the first provider in the chain is Copilot, unless the bead is tagged "decompose". (Forge-qkn6)
