category: Added
- **Interactive selection prompts for update-deps** - The `forge update-deps` command now displays an interactive prompt after the scan summary: `[a]ll / [p]atch+minor only / [s]elect groups / [n]o`. Use `--dry-run` to preview grouped updates without prompting; `--patch-only` and `--no-major` pre-filter groups before the prompt is shown. (Forge-uj67)
