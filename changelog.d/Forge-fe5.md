category: Fixed
- **extractJSON now accepts a requiredKey parameter** - Previously hardcoded to only match JSON containing "verdict", making it unusable for non-verdict JSON parsing (e.g., rule distillation). The key is now matched as a quoted JSON key to avoid false positives from occurrences inside string values. (Forge-fe5)
