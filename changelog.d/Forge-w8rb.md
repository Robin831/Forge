category: Fixed
- **Temper hooks fire in burnish and quench** - Pipeline stage hooks (`before_temper`, `after_temper`) now fire during burnish (review-fix) and quench (CI-fix) temper runs, not just during the initial pipeline. Setup commands like `npm ci` now apply uniformly across all temper invocations. (Forge-w8rb)
