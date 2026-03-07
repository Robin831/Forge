category: Fixed
- **extractJSON now accepts a requiredKey parameter** - Previously hardcoded to only match JSON containing "verdict", making it unusable for non-verdict JSON parsing (e.g., rule distillation). Callers now pass the key they need. (Forge-fe5)
