category: Fixed
- **Fix install.sh checksum grep matching `.sbom.json` sidecar files** - Anchored the grep pattern to `" ${ASSET_NAME}$"` so it matches only the exact archive filename at end of line, preventing multi-line `EXPECTED_HASH` and false SHA256 mismatch errors. (Forge-dras)
