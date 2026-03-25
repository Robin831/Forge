category: Changed
- **vulncheck: modernize string/slice patterns** - Replace `bytes.Split`+range with `bytes.SplitSeq` iterator and manual duplicate-check loop with `slices.Contains` in the govulncheck JSON parser. (Forge-q66l)
