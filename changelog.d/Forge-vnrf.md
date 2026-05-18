category: Added
- **`useUIState` persistence hook** - New `internal/web/frontend/src/hooks/useUIState.ts` typed wrapper over sessionStorage/localStorage with JSON codec, ~150ms debounced writes, SSR-safe init, and a `clearAll(prefix?)` export for tests. Foundational primitive for remembering pane-local UI state (sort orders, expanded rows, filters) across routes and per user. (Forge-vnrf)
