category: Fixed
- **Bump bd subprocess timeouts to 60s** - All bd subprocess invocations now use a centralized 60-second timeout (up from 10-30s) via `executil.DefaultBdTimeout`, preventing premature kills on anvils with remote Dolt or GitHub auto-sync where bd writes routinely take 20-30 seconds. (Forge-u21i)
