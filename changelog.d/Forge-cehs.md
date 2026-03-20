category: Fixed
- **Depcheck npm scanner now syncs node_modules before scanning** - Runs `npm install --ignore-scripts` before `npm outdated` so reported versions match the lock file instead of potentially stale installed versions. (Forge-cehs)
