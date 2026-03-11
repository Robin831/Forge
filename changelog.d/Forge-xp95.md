category: Added
- **Toast notifications in Hearth TUI** - Transient toast messages now appear at the bottom of the dashboard for key events: PR created, PR merged, bead closed, warden review passed, smith failure, PR merge failure, lifecycle exhausted, and crucible complete. Toasts auto-dismiss after 4 seconds using `tea.Tick` and stack up to 3 at once. (Forge-xp95)
