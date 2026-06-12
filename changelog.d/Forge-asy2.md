category: Fixed
- **Cap Assay reviews per PR** - Assay re-fired on every new head SHA, so the Assay→Burnish→new-head loop ran until a pass found nothing. The Bellows trigger now stops after `assay.max_runs` executed reviews per PR (default 2; skipped runs don't count; 0 disables the cap). (Forge-asy2)
