category: Fixed
- **Hearth header no longer renders outside visible area** - Fixed off-by-two in the TUI height calculation that caused the total view to exceed terminal height by 2 lines, pushing the header off-screen in PowerShell. The vertical split now correctly accounts for panel border lines. (Forge-vf6)
