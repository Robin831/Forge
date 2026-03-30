category: Fixed
- **Lifecycle worker timeout** - Burnish (review fix), quench (CI fix), and rebase workers now run under a deadline derived from `smith_timeout`, preventing indefinite hangs when a worker stalls. (Forge-hvta)

