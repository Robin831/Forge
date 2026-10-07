category: Fixed
- **Burnish's review-fix verification no longer times out while queued for the dotnet lock** - Time Temper spends waiting for another worker's dotnet build or test now moves burnish's overall verification deadline out by the same amount, as #929 already did for each step's own timeout; genuinely slow work still times out on schedule. (Fhi.Metadata-8py4x)
