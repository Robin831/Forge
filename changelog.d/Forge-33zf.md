category: Added
- **Support 'no changes needed' as valid Smith outcome** - When Smith determines no code changes are required (e.g. the fix is already implemented or resolved upstream), it can now signal this via the NO_CHANGES_NEEDED: marker. The pipeline skips Warden and Temper, closes the bead with the reason, and logs a dedicated no_changes_needed event. (Forge-33zf)
