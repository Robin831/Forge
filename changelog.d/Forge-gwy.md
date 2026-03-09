category: Fixed
- **Hot-reload: fix fsnotify losing watch after file replacement** - Watch the parent directory instead of the config file directly so that editors using write-temp-then-rename (common on Windows) no longer silently break hot-reload. (Forge-gwy)
