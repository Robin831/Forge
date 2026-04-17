category: Changed
- **install.ps1 unblocks downloaded binary on Windows** - Call `Unblock-File` after extracting `forge.exe` to strip the Zone.Identifier NTFS stream that triggers repeated Windows Defender scans. Wrapped in try/catch so installation continues if the call fails. (Forge-9u3j)
