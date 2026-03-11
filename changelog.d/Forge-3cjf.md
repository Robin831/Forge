category: Added
- **tar.gz archives for Linux and macOS releases** - GoReleaser now produces `.tar.gz` archives for Linux and macOS targets in addition to `.zip` for Windows. The `install.sh` script has been updated to use `tar` (universally available) instead of `unzip`, removing the need to install an extra package on minimal containers. (Forge-3cjf)
