category: Fixed
- **Allow all lifecycle actions on non-bead PRs** - The BeadID guard was too broad, blocking burnish/quench/rebase on PRs with no associated bead (e.g. warden-learn PRs). Added IsManual flag so both automatic and manual actions work on non-bead PRs. (Forge-3l3w)
