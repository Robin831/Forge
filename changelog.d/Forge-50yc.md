category: Fixed
- **Ledger crash on out-of-range timestamps** - Sanitise `closed_at`/`updated_at` timestamps from `bd list --json` whose year is outside the JSON-safe range [0,9999], preventing a panic when the Ledger re-marshals them for the detail panel. (Forge-50yc)

