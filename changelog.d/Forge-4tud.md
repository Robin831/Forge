category: Added
- **Persistent UI state across Hearth panes** - Queue, Workers, Live Activity, and PR panes now remember their filter, sort, expansion, and scroll state across navigation, with the `useUIState` hook deciding per pane whether each slice survives a browser restart (localStorage) or just in-tab navigation (sessionStorage). (Forge-4tud)
