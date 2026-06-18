category: Added
- **Settings page in Hearth dashboard** - New `/settings` page that lists the managed boolean pipeline settings grouped by area, with an accessible toggle per setting. Toggles update optimistically and revert on error via `PATCH /api/forge/config`, and non-hot-reloadable settings show an "applies on next run" note. (Forge-9qzi)
