category: Changed
- **Reduced smith re-exploration on warden/temper feedback iterations** - On iteration 2+, the prompt now includes the previous iteration's diff at the top with a directive to skip codebase re-exploration, significantly reducing wasted tool calls. (Forge-zfok)
