category: Fixed
- **Bump bd subprocess timeout to 5 minutes** - Increase `executil.DefaultBdTimeout` from 60 seconds to 5 minutes to prevent premature kills on anvils with remote Dolt, kubectl port-forward, or GitHub auto-sync under concurrent load. (Forge-apqe)
