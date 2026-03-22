category: Fixed
- **Ledger bead filter always empty** - Fixed 'No beads match the current filter' in the Ledger by replacing single `bd list --status=open --status=in_progress` calls (unsupported by bd) with separate per-status calls in `FetchAnvilBeads` and `FetchAllBeads`. (Forge-g9h2)
