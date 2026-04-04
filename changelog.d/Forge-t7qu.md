category: Fixed
- **Decrypt enc: webhook URLs from config** - Forge now decrypts AES-256-GCM encrypted webhook URLs (enc: prefix written by Hytte) when loading config, eliminating 'unsupported protocol scheme "enc"' errors on every webhook delivery. (Forge-t7qu)
