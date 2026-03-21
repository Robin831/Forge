category: Fixed
- **depcheck dedup timeouts for remote Dolt anvils** - Increased `bd list` timeout from 15s to 60s and `bd show` timeout from 10s to 30s so anvils using a remote Dolt server (e.g. via kubectl port-forward) no longer skip bead creation. (Forge-6h1q)
