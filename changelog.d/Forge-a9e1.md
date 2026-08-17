category: Changed
- **gofmt-clean `internal/hearth/hearth.go`** - The file had accumulated stale const-block and struct-field alignment that `gofmt` wanted to rewrite, so `gofmt -l` flagged it and the repo's formatting gate was meaningless for it. Applied `gofmt -w` as its own commit — whitespace only, no behavior change. (Forge-a9e1)
