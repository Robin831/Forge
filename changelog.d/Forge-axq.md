category: Changed
- **Refactored depcheck into multi-ecosystem scanner** - The Go dependency update checker has been restructured from a Go-only Monitor into a Scanner pattern that supports multiple ecosystems. Go-specific logic is now in `go.go`, with shared dedup logic and a `ScanAll()` method used by the Go, npm (`npm.go`), and .NET/NuGet (`dotnet.go`) scanners. (Forge-axq)
