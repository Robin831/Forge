category: Added
- **Config file and disk space doctor checks** - `forge doctor` now warns about missing or unreadable config files, world-readable permissions (Unix), and low disk space (<1 GiB) on the forge directory and anvil volumes. Added `--strict` flag to treat warnings as failures. The release formula now runs `forge doctor --strict` as a pre-release gate. (Forge-cxwi)
