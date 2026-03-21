category: Added
- **Error toasts & bd command failure handling** - Added toast notification system to Ledger and Hearth TUIs: `toast` struct, `toastDismissMsg`, tick-based auto-dismiss, and ANSI-aware overlay rendering. All bd command errors (`ActionErrorMsg`, kanban lane moves) now surface as dismissing toast overlays instead of silent footer updates. (Forge-1lj7)
