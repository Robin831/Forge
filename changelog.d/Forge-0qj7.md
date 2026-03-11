category: Fixed
- **Decomposed flag no longer clears retry record when no children created** - When schematic ran with `ActionDecompose` but produced zero sub-beads, the daemon incorrectly cleared the retry record, causing the bead to silently disappear instead of surfacing in Needs Attention. Now the retry record is only cleared when actual child beads were created. (Forge-0qj7)
