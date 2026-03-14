category: Fixed
- **Warden rules YAML quoting** - Auto-learned rules containing colon-space (`: `) in values are now double-quoted when saved, preventing YAML parse errors on reload that previously broke all rule loading for the anvil. (Forge-dkty)
