category: Fixed
- **Ledger bd close false error** - `bd close --json` sometimes exits with status 1 even when the close succeeded. Ledger now inspects the JSON output and treats the operation as a success when the returned bead has `status=closed` with a valid `closed_at` timestamp, preventing spurious error messages in the activity log. (Forge-hbxz)

