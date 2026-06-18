category: Added
- **Forge config read/write API** - New `GET /api/forge/config` returns the managed boolean settings with per-key metadata (value, default, doc string, area, label, and a hotReloadable flag), and `PATCH /api/forge/config` persists one or more of them to `~/.forge/config.yaml` via a comment-preserving YAML node-tree edit so the daemon hot-reloads. (Forge-e4xe)
