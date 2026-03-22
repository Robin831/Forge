category: Fixed
- **Depcheck scan stdout suppression** - `runNpmInstall` in the depcheck scan phase now discards stdout (`io.Discard`) so npm ci output no longer leaks to the terminal and changes the tab title. (Forge-bjxm)
- **Hearth window title after update** - The `updateApplyDoneMsg` handler now resets the window title to "The Forge — Hearth" after dependency updates complete, restoring the correct tab title. (Forge-bjxm)
- **Live Activity plain-text log lines** - The log tailer now surfaces plain-text lines (e.g. from the synthetic depupdate worker) in the Live Activity panel instead of silently dropping them. (Forge-bjxm)
