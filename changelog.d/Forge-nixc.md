category: Fixed
- **Force smith now continues with temper/warden/PR instead of re-dispatching** - After force smith completes, the pipeline now proceeds directly to temper verification, warden review, and PR creation on the same branch instead of resetting the bead to open and re-running the full pipeline from scratch. The bead stays in_progress throughout. (Forge-nixc)
