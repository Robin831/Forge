category: Fixed
- **Orphan recovery no longer resets beads claimed by humans or external tools** - The orphan recovery logic now checks for an existing worker record in Forge's state DB before resetting an in_progress bead to open. Beads that were never claimed by Forge (e.g., worked on by a human or Copilot) are left untouched. (Forge-zwt)
