category: Fixed
- **CLI now resolves `~/.forge/config.yaml`** - `forge anvil list` (and any other CLI command that loads config without `--config`) was searching for `~/.forge/forge.yaml` while the documented path and the actual deployed file is `~/.forge/config.yaml`. Resolution now matches the documented order: `--config` flag, then `./forge.yaml`, then `~/.forge/config.yaml`. (Forge-9mka)
