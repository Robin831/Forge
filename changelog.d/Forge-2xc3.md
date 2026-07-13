category: Fixed
- **Dispatch pause survives daemon restarts** - `forge pause` (and the web Pause toggle) now persists the dispatch-pause flag in state.db and restores it on startup, so a restart no longer silently resumes auto-dispatch. Status and the web banner now also show when the pause started. (Forge-2xc3)
