category: Fixed
- **Ledger closed-bead fetch timeout** - Cap `bd list --status=closed` at 50 results (instead of unlimited) and increase fetch timeout to 60s to prevent timeouts on remote Dolt anvils over slow connections. (Forge-jej7)

