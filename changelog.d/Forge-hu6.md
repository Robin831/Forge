category: Added
- **PowerShell install script** - `install.ps1` enables one-line installation via `irm .../install.ps1 | iex`. Detects OS/arch, downloads the latest release, verifies checksum, extracts to a sensible location, and adds to PATH. Supports updates and pinning to a specific version. (Forge-hu6)
