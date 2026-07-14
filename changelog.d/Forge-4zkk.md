category: Fixed
- **Windows orphan worker containment** - Spawned workers are now assigned to a kill-on-close Job Object so they are reaped when the daemon exits, and the previously no-op Windows orphan sweep now enumerates processes via a Toolhelp snapshot and reaps pre-crash strays verified by PID + creation time. (Forge-4zkk)
