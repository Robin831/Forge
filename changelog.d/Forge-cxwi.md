category: Added
- **Config file and disk space doctor checks** - `forge doctor` now warns about missing or unreadable config files, world-readable permissions (Unix), and low disk space (<1 GiB) on the forge directory and anvil volumes. The release formula now runs `forge doctor` as a pre-release sanity check. (Forge-cxwi)
