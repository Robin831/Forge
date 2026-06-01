category: Added
- **Assay PR posting layer** - When Assay runs outside shadow mode it now posts a top-level summary review with a severity table, opens one inline review comment per finding (anchored to the head SHA and tagged with a stable `assay-hash` marker), and auto-resolves a finding's review thread after two consecutive reviews fail to re-detect it. (Forge-g197)
