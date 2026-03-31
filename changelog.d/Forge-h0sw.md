category: Fixed
- **Hearth poll health badges missing when config not in working directory** - When `forge hearth` is run from a directory without `forge.yaml` (e.g. SSH sessions on remote hosts), the anvil list was empty so poll health badges were never shown. Hearth now falls back to querying all known anvils from the events table when no config-derived anvil list is available. (Forge-h0sw)

