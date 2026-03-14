category: Fixed
- Depcheck no longer creates duplicate beads when the beads database is unreachable — the dedup cache now tracks validity and skips bead creation when `bd list` fails, instead of silently treating failures as "no beads exist"
