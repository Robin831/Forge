category: Fixed
- **Wicket bead creation fails with "unknown flag: --tag"** - The `bd` CLI renamed `--tag` to `--labels` (for create) and `--add-label` (for update), but Wicket still used the old flag names. Updated all references. (Forge-wicket-tag-fix)
