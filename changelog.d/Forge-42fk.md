category: Added
- **Changelog fragment format validation** - `forge changelog validate` (no args) now reports all malformed fragments instead of stopping at the first error. The release formula runs this check before assembly so bad fragments are caught early with clear error messages. (Forge-42fk)
