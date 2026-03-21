category: Added
- **Auto-close matching dep beads after PR creation** - After `forge update-deps` creates a batch update PR, any open depcheck beads covering the same packages are automatically closed with the reason "Updated via forge update-deps", preventing the queue from filling with resolved work. (Forge-3cx3)

