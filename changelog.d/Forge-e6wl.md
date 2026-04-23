category: Fixed
- **Warden no longer rejects EF Core migrations as scope drift** - Auto-generated files (`**/Migrations/*.Designer.cs`, `**/Migrations/*ModelSnapshot.cs`) are stripped from the diff before review, and the remaining diff cap was raised from 50KB to 250KB so legitimate large migrations still fit without truncation. (Forge-e6wl)
