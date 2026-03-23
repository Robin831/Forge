category: Fixed
- **Subprocess console isolation** - Use CREATE_NO_WINDOW flag on Windows to fully prevent subprocesses from corrupting the terminal tab title via Console API calls. (Forge-bjxm)
- **TUI log output suppression** - Redirect Go's default logger to io.Discard while Hearth/Ledger TUI is running to prevent background goroutine log output from corrupting the alt-screen. (Forge-bjxm)
- **Depupdate gh CLI compatibility** - Remove unsupported --json flag from gh pr create for older gh versions. (Forge-9f1g)
- **Depupdate remote branch cleanup** - Delete remote dep branch before pushing to avoid stale-info rejection on same-day re-runs. (Forge-9f1g)
- **Depupdate bead auto-close** - Close matching depcheck beads after PR creation in both Hearth and Ledger update overlays. (Forge-9f1g)
- **Depupdate changelog format** - Generate changelog fragments in bold-title format with traceability tags, with proper Norwegian translations for bilingual repos. (Forge-9f1g)
- **Ledger bd list timeout** - Increase bd command timeout from 30 seconds to 3 minutes for slow Dolt connections. (Forge-g9h2)
