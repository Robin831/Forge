category: Added
- **`forge update` command for self-updating the binary** - Downloads the latest release from GitHub, verifies the checksum if a checksums file is present, gracefully stops the daemon, replaces the binary (with a `.bak` rollback on failure), and restarts the daemon. Works without Go installed. `forge status` now hints when a newer release is available. (Forge-1xir)
