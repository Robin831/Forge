category: Fixed
- **npm install runs in correct directory for multi-package.json repos** - `InstallNpmGroup` now uses the `SourceDir` recorded on each `ModuleUpdate` (the directory where the `package.json` was found) instead of always running from the anvil root. This prevents spurious root `node_modules/` creation and ensures the right `package-lock.json` is updated when a repo has multiple `package.json` files (e.g. root and `web/`). (Forge-k04c)

