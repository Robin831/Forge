category: Fixed
- **Decomposed child beads now inherit the parent's auto_dispatch tag** - When Schematic decomposes a bead into children, the daemon now copies the `forgeReady` label (or whatever `auto_dispatch_tag` is configured) from the parent to each child, so they are picked up by the poller immediately instead of sitting in the queue forever. (Forge-fk5f)
