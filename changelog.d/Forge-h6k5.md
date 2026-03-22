category: Fixed
- **Depupdate stdout suppression** - Add io.Discard to all git and bd subprocess calls in depupdate that were missing stdout capture, preventing terminal tab title corruption and TUI display artifacts during dependency updates. (Forge-h6k5)
