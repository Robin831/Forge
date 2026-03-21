category: Added
- **Programmatic depupdate API** - Expose `Scan`, `Apply`, and `Preview` functions in `internal/depupdate` so Hearth and Ledger can query and apply dependency updates without going through the CLI. Introduces `Anvil`, `AnvilReport`, and `Result` types for structured, testable integration. (Forge-815c)
