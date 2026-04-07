category: Fixed
- **Worker kill: 2-phase SIGINT→SIGKILL with process group support** - Kill worker via IPC now sends SIGINT to the entire process group, waits up to 5 seconds, then escalates to SIGKILL. This ensures child processes (git, node, etc.) are also terminated. (Forge-yx7k)
