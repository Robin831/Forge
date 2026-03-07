category: Fixed
- **Temper: LoadAnvilConfig properly surfaces non-ENOENT errors** - Permission errors and corrupt YAML in `.forge/temper.yaml` are now returned to callers instead of being silently swallowed. The daemon's `loadAnvilTemperCached` also caches non-ENOENT stat errors to avoid log spam on every dispatch. (Forge-kna)
