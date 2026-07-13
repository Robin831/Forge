category: Fixed
- **Logsweep protects paused workers' preserved logs** - The log retention sweep now unions paused workers into its active-bead set, so a bead parked by an operator pause keeps its `~/.forge/logs/<bead>/` transcript history regardless of age until it is resumed. (Forge-833f)
