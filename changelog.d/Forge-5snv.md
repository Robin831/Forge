category: Fixed
- **Stopping a bead from the web GUI now releases its bd claim** - The web/Hearth stop action (queue_stop) previously left the bead claimed and in_progress in bd, hiding it from `bd ready` until manual cleanup. Both stop verbs now share one implementation that returns the bead to open with the assignee cleared, so a UI stop behaves identically to a CLI stop. (Forge-5snv)
