category: Fixed
- Depcheck now runs `git pull --ff-only` before scanning each anvil, preventing duplicate beads for dependencies that were already updated on main but not yet pulled locally
