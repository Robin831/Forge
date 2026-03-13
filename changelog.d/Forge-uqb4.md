### Changed
- Hearth Queue panel: anvil headers now show health badges (● green = last poll OK, ⊘ red = poll error) with time since last poll. For single-anvil setups, the badge appears in the panel title.
- Hearth Events panel: poll/poll_error events are no longer shown in the event log since anvil health is now visible in the Queue panel.

### Added
- `state.LastPollPerAnvil()` query for efficient per-anvil health status lookup.
