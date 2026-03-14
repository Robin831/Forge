category: Fixed
- Depcheck no longer creates duplicate beads when the beads database is unreachable — the dedup cache now tracks validity and skips bead creation when `bd list` fails, instead of silently treating failures as "no beads exist"
- Depcheck dedup cache now fetches all beads (`--limit 0`) instead of defaulting to 50, preventing duplicates when more than 50 open beads exist
- NuGet depcheck deduplicates packages across multiple .sln/.csproj files in the same anvil, reducing false "outdated" counts
