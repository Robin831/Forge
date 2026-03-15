category: Changed
- **Incremental log reading in Hearth** - Worker log files are now tailed incrementally instead of re-reading the entire file every 2-second tick, significantly reducing I/O for long-running workers. (Forge-80ro)
