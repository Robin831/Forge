category: Fixed
- **Schematic tolerates trailing noise in bd output** - Use streaming JSON decoder instead of strict `json.Unmarshal` so that trailing diagnostics from `bd create --json` (e.g. orphan detection warnings) no longer break sub-bead ID parsing. Applied the same fix to depcheck and ledger packages. (Forge-byca)
