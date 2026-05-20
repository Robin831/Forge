category: Added
- **Anvil validation on Beads-Forge session create** - Reject `POST /api/forge/sessions` with 400 when the supplied anvil does not match any registered anvil; auto-populate the anvil when exactly one is registered and none was specified; reject with a clear error when several are registered and no choice was made. (Forge-4avf)
