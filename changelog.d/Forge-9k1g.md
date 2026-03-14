category: Added
- **X-Forge-Event HTTP header on webhook notifications** - Generic JSON webhook requests now include an `X-Forge-Event` header set to the event type (e.g. `pr_created`, `bead_failed`, `release_published`). This allows consumers like Hytte to identify and filter Forge-originated webhooks without parsing the JSON body. (Forge-9k1g)
