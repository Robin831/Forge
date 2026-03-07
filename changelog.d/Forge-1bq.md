category: Added
- **Semantic comment grouping in Warden learn** - GroupComments now clusters semantically similar Copilot review comments using keyword overlap (Jaccard similarity) instead of requiring exact text matches. Comments like "missing error check on Open()" and "error from ReadFile not handled" are now grouped together for rule distillation. (Forge-1bq)
