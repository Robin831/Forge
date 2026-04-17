category: Fixed
- **resetBead timeout too short for slow Dolt connections** - Bumped resetBead timeout from 10s to 5min (matching other bd write sites), surface "context deadline exceeded" in error messages, and skip incrementing the recovery failure counter on transient timeouts so they don't trip the circuit breaker. (Forge-4omg)
