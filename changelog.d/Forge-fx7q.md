category: Added
- **Warden learns from CI fix patterns** - After a successful `cifix`, Forge now extracts ESLint rule IDs from the failing CI logs, distills them into warden rules (via Claude), and stores them in `.forge/warden-rules.yaml`. This allows the Warden to flag the same anti-pattern during code review before it ever hits CI, reducing cifix cycles from 5-9 down to 0-1. (Forge-fx7q)
