category: Fixed
- **Flaky combined persistence tests** - Fix timeout in WorkersPane and PRsPage persistence tests by disabling userEvent's inter-event setTimeout under fake timers via `delay: null`. (Forge-admm)
