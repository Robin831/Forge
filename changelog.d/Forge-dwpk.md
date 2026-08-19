category: Changed
- **One pluralization helper** - Folded the four package-local plural helpers (CLI previews, questgiver reports, the web layer's bead messages, the daemon's pause status) and the open-coded pluralization in the merged-but-unclosed-bead notice into a single `internal/textfmt` package. Rendered output is unchanged. (Forge-dwpk)
