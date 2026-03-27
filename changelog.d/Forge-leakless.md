category: Fixed
- **Windows Defender no longer flags Adventurer/Rod** - Disabled the leakless helper binary (`launcher.Leakless(false)`) that Rod extracts at runtime, which Windows Defender flagged as suspicious. Process cleanup is handled by context cancellation instead. (Forge-leakless)
