category: Added
- **Provider `--resume` session support** - `Provider.BuildArgsResume` builds Claude one-shot args that inject `--resume <session_id>` before the `-p` flag to continue a prior session, and returns a clear error for Gemini/Copilot/OpenAI providers, which have no resumable session id. (Forge-4c4f)
