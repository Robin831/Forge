category: Changed
- **Repo-wide gofmt under the go1.26 toolchain** - Reformatted 45 files with `gofmt -w` to clear accumulated column-alignment and trailing-whitespace drift, so the formatting gate stays meaningful and future diffs are not noise. No behavior changes. (Forge-z5el)
