category: Fixed
- **Warden: default to approve when verdict cannot be parsed** - Unparseable warden output now defaults to `VerdictApprove` instead of `VerdictRequestChanges`, preventing a wasted Smith iteration on parse failures. (Forge-pax)
