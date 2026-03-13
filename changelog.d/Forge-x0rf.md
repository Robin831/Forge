category: Added
- **Native Linux packages (deb, rpm, apk) via GoReleaser nfpms** - Releases now produce deb, rpm, and apk packages enabling installation via `apt install forge` or `yum install forge`. Includes a systemd unit file (`forge.service`) that is enabled on install, starting the daemon automatically after installation. (Forge-x0rf)
