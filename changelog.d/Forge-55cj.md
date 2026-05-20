category: Added
- **Required anvil selector on Beads-Forge new-session form** - The new-session form now lists registered anvils (fetched from `GET /api/forge/anvils`), defaults to the user's last-used anvil from localStorage, disables submission until an anvil is chosen, and surfaces an actionable empty-state message when no anvils are registered. (Forge-55cj)
