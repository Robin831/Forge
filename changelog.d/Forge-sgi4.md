category: Fixed
- **Depcheck duplicate bead creation** - Fixed an issue where depcheck created a new "Package updates" bead each day instead of reusing the existing open one. The bead lookup now matches any open bead with the "Package updates" title prefix rather than requiring an exact date-specific title match. (Forge-sgi4)

