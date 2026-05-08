category: Fixed
- **Hearth 2.0 dashboard crash on empty crucibles** - The `/api/crucibles` endpoint now serialises an empty list as `[]` instead of `null`, and the dashboard's stat-card readout null-guards `crucibles.length` so a fresh deploy with no active crucibles renders correctly. (Forge-76hw)
