category: Added
- **Resolve store and Forge API client** - Hearth 2.0 frontend gains a typed `src/api/forge.ts` client for `GET /api/forge/escalation/<bead>` and `POST /api/forge/resolve`, plus a `src/stores/resolveStore.tsx` slice tracking in-flight resolve requests (idle/pending/success/error) keyed by bead/anvil/worker. Consumed by the upcoming resolve-needs-attention panel. (Forge-hxnn)
