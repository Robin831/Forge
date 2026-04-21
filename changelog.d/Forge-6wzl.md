category: Fixed
- **DecodeJSON tolerates leading bracket noise from bd** - `executil.DecodeJSON` now scans every `{` (then every `[`) candidate position and decodes the first one that succeeds, so output like `[mysql] ... i/o timeout` followed by valid JSON no longer breaks schematic decomposition. Errors when no JSON is found include a bounded snippet of the raw output. (Forge-6wzl)
