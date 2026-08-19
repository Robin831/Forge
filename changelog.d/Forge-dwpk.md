category: Changed
- **One pluralization helper** - Folded the four package-local plural helpers (CLI previews, questgiver reports, the web layer's bead messages, the daemon's pause status) and the two open-coded copies in the daemon (the merged-but-unclosed-bead notice and the preview resource note) into a single `internal/textfmt` package. Rendered output is unchanged. (Forge-dwpk)
