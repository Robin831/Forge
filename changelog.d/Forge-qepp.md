category: Fixed
- **forge update binary not found** - Fixed `forge update` failing with "no release binary found" by matching the GoReleaser archive naming convention (`forge_{version}_{os}_{arch}.zip|tar.gz`) and extracting the binary from the downloaded archive. (Forge-qepp)
