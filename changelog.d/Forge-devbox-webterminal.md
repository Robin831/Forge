category: Added
- **Devbox browser-terminal tooling** - Bundle `ttyd` and `tmux` in the image so the skybert devbox can serve a web terminal (xterm.js over websocket) behind an authed ingress — more resilient than `kubectl exec`, whose stream gets cut by API-server idle timeouts. Unused by the Forge daemon itself. (Forge-devbox-webterminal)
