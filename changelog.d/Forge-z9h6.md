category: Fixed
- **Fix false empty-diff detection after smith pushes** - The hasEmptyDiff check now compares against the pre-smith HEAD SHA instead of @{upstream}...HEAD, preventing false needs_human escalation when smith commits and pushes successfully. (Forge-z9h6)
