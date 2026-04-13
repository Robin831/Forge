category: Added
- **Temper path-based step filtering** - Temper steps can now include an optional `paths` glob filter. When set, the step is skipped if no changed files in the diff match the patterns, saving time on multi-stack repos where not all steps are relevant to every change. (Forge-p0sa)
