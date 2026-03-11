category: Added
- **One-liner install script for Linux and macOS** - Added `install.sh` at the repo root that detects OS/arch, fetches the latest (or a pinned) release from GitHub, verifies the SHA256 checksum, and installs the `forge` binary to `~/bin`. The GoReleaser release body now includes the install command so it appears on every GitHub release page. (Forge-t3ba)
