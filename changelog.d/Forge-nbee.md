category: Fixed
- **Orphan recovery no longer resets beads parked for human attention** - Beads with `needs_human=1` or `clarification_needed=1` in the retries table are now skipped during orphan recovery, preventing them from being re-dispatched without the human intervention they require. (Forge-nbee)
