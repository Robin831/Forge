category: Fixed
- **Ingot panel now distinguishes skipped temper steps** - Path-skipped steps (e.g. client-* on a backend-only diff) previously rendered as `pass · 0.0s · exit 0`, indistinguishable from steps that actually ran. The temper `Skipped` flag is now persisted on ingot test results and surfaced in Hearth as a muted `skip` verdict with dashed-out duration and exit columns. (Forge-uzy1)
