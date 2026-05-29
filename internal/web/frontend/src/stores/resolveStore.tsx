import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { ApiError } from '../api'
import {
  fetchEscalation as apiFetchEscalation,
  postResolve,
  type EscalationDetail,
  type ResolveRequest,
  type ResolveVerb,
} from '../api/forge'

// ResolveStatus is the lifecycle of a single resolve request. Mirrors the
// states the panel buttons render: idle (no recent attempt), pending
// (in-flight POST, button disabled + spinner), success (recent OK), error
// (last attempt failed, surface the message). State is updated via direct
// setEntries calls: calling run on an existing key always sets it to pending
// first, regardless of its previous status.
export type ResolveStatus = 'idle' | 'pending' | 'success' | 'error'

// ResolveEntry is the per-key state surfaced to consumers. `verb` records
// which verb produced the current status so panels with multiple buttons
// know which one to highlight; `error` is the daemon's message for the
// last failed attempt (cleared on success).
export interface ResolveEntry {
  status: ResolveStatus
  verb?: ResolveVerb
  error?: string
  updatedAt: number
}

// ResolveKey is what callers use to identify an in-flight request. Most
// resolve verbs operate on a bead within an anvil; when the panel is
// rendered for a worker-level escalation the caller can fold the worker_id
// into the key so two simultaneous worker resolutions on the same bead
// don't share state. The helper below builds a canonical key.
export type ResolveKey = string

// resolveKey produces a stable key from a bead/anvil pair, optionally
// scoped to a worker. Centralising it means consumers can compute the key
// the same way the panel does without duplicating the format.
export function resolveKey(
  beadID: string,
  anvil: string,
  workerID?: string,
): ResolveKey {
  const base = `${anvil}/${beadID}`
  return workerID ? `${base}#${workerID}` : base
}

const IDLE: ResolveEntry = { status: 'idle', updatedAt: 0 }

// EscalationStatus is the lifecycle of a single fetchEscalation call.
// 'idle' means the panel has not yet asked for this escalation; consumers
// rendering on mount will briefly see this before the useEffect fires.
export type EscalationStatus = 'idle' | 'loading' | 'success' | 'error'

// EscalationEntry is the per-id state surfaced to consumers. data is the
// last successful response (kept across re-fetches so refreshes don't
// flash an empty skeleton); error is the message from the last failed
// attempt and is cleared on success.
export interface EscalationEntry {
  status: EscalationStatus
  data?: EscalationDetail
  error?: string
  updatedAt: number
}

const IDLE_ESCALATION: EscalationEntry = { status: 'idle', updatedAt: 0 }

// escalationKey produces a stable cache key from a bead/anvil pair. When anvil
// is omitted the key degrades to just the bead id (legacy worker-row path where
// the anvil is already unambiguous). Centralising it means the store and
// consumers agree on the key shape.
export function escalationKey(beadID: string, anvil?: string): string {
  return anvil ? `${anvil}/${beadID}` : beadID
}

interface ResolveActions {
  // run dispatches a verb to POST /api/forge/resolve and tracks the
  // result against the supplied key. Resolves to true on success, false
  // on failure. The store transitions to pending immediately so consumers
  // observe disabled buttons before the network call settles.
  run: (key: ResolveKey, beadID: string, req: ResolveRequest) => Promise<boolean>
  // reset clears the entry for a key back to idle. Useful after the
  // operator dismisses an error toast or navigates away.
  reset: (key: ResolveKey) => void
  // setEntry is the low-level setter exposed for tests and for the rare
  // case a consumer wants to seed a key (e.g. record an externally-driven
  // success). Production code should prefer `run`.
  setEntry: (key: ResolveKey, entry: ResolveEntry) => void
  // fetchEscalation loads the escalation detail for a bead via
  // GET /api/forge/escalation/{bead_id} and stores it under escalationId.
  // The store transitions to 'loading' immediately so consumers can render
  // a skeleton, then to 'success' or 'error'. Resolves to the detail (or
  // null on error) so callers awaiting the result can branch.
  fetchEscalation: (
    escalationId: string,
    anvil?: string,
  ) => Promise<EscalationDetail | null>
}

interface ResolveStoreValue {
  // entries is the full map of in-flight + recently-resolved keys. Most
  // consumers should reach for `useResolveStatus(key)` which selects a
  // single entry; this is exposed for tests and for panels that need to
  // render a bulk view.
  entries: Record<ResolveKey, ResolveEntry>
  // escalations is the full map of escalation detail entries keyed by the
  // escalationId passed to fetchEscalation. Most consumers should use
  // `useEscalation(id)`; this is exposed for tests and bulk views.
  escalations: Record<string, EscalationEntry>
  actions: ResolveActions
}

const ResolveStoreContext = createContext<ResolveStoreValue | null>(null)

// ResolveStoreProvider wires the resolve state into the React tree. It is
// mounted once near the root (next to ToastProvider) so any panel can call
// `useResolveStatus` / `useResolveActions` without prop-drilling.
export function ResolveStoreProvider({ children }: { children: ReactNode }) {
  const [entries, setEntries] = useState<Record<ResolveKey, ResolveEntry>>({})
  const [escalations, setEscalations] = useState<
    Record<string, EscalationEntry>
  >({})

  const setEntry = useCallback((key: ResolveKey, entry: ResolveEntry) => {
    setEntries((prev) => ({ ...prev, [key]: entry }))
  }, [])

  const reset = useCallback((key: ResolveKey) => {
    setEntries((prev) => {
      if (!(key in prev)) return prev
      const next = { ...prev }
      delete next[key]
      return next
    })
  }, [])

  const run = useCallback(
    async (
      key: ResolveKey,
      beadID: string,
      req: ResolveRequest,
    ): Promise<boolean> => {
      setEntries((prev) => ({
        ...prev,
        [key]: { status: 'pending', verb: req.verb, updatedAt: Date.now() },
      }))
      try {
        await postResolve(beadID, req)
        setEntries((prev) => ({
          ...prev,
          [key]: { status: 'success', verb: req.verb, updatedAt: Date.now() },
        }))
        return true
      } catch (err) {
        const message =
          err instanceof ApiError
            ? err.message
            : err instanceof Error
              ? err.message
              : 'request failed'
        setEntries((prev) => ({
          ...prev,
          [key]: {
            status: 'error',
            verb: req.verb,
            error: message,
            updatedAt: Date.now(),
          },
        }))
        return false
      }
    },
    [],
  )

  const fetchEscalation = useCallback(
    async (
      escalationId: string,
      anvil?: string,
    ): Promise<EscalationDetail | null> => {
      const key = escalationKey(escalationId, anvil)
      // Preserve any previously-loaded data on the entry so a refetch
      // does not flash a skeleton in the UI; the status flag is what
      // consumers observe to render a loading indicator.
      setEscalations((prev) => {
        const existing = prev[key]
        return {
          ...prev,
          [key]: {
            status: 'loading',
            data: existing?.data,
            updatedAt: Date.now(),
          },
        }
      })
      try {
        const detail = await apiFetchEscalation(escalationId, anvil)
        setEscalations((prev) => ({
          ...prev,
          [key]: {
            status: 'success',
            data: detail,
            updatedAt: Date.now(),
          },
        }))
        return detail
      } catch (err) {
        const message =
          err instanceof ApiError
            ? err.message
            : err instanceof Error
              ? err.message
              : 'request failed'
        setEscalations((prev) => ({
          ...prev,
          [key]: {
            status: 'error',
            data: prev[key]?.data,
            error: message,
            updatedAt: Date.now(),
          },
        }))
        return null
      }
    },
    [],
  )

  // actions is memoised independently of entries so that useResolveActions()
  // returns a stable object reference across renders — safe to include in
  // effect dependency lists without triggering spurious re-runs.
  const actions = useMemo<ResolveActions>(
    () => ({ run, reset, setEntry, fetchEscalation }),
    [run, reset, setEntry, fetchEscalation],
  )

  const value = useMemo<ResolveStoreValue>(
    () => ({ entries, escalations, actions }),
    [entries, escalations, actions],
  )

  return (
    <ResolveStoreContext.Provider value={value}>
      {children}
    </ResolveStoreContext.Provider>
  )
}

function useResolveStore(): ResolveStoreValue {
  const value = useContext(ResolveStoreContext)
  if (!value) {
    throw new Error(
      'useResolveStore: ResolveStoreProvider missing from the React tree',
    )
  }
  return value
}

// useResolveStatus selects a single entry from the store. Missing keys
// resolve to the shared IDLE constant so consumers can destructure
// `status` without a null check on every render.
// eslint-disable-next-line react-refresh/only-export-components
export function useResolveStatus(key: ResolveKey): ResolveEntry {
  const { entries } = useResolveStore()
  return entries[key] ?? IDLE
}

// useResolveActions returns the imperative API. The returned object is
// stable across renders (memoised by the provider) so it is safe to
// include in effect dependency lists.
// eslint-disable-next-line react-refresh/only-export-components
export function useResolveActions(): ResolveActions {
  return useResolveStore().actions
}

// useResolveEntries exposes the full map. Reserved for tests and for the
// (rare) panel that wants to enumerate every in-flight key — most
// production callers should use useResolveStatus instead.
// eslint-disable-next-line react-refresh/only-export-components
export function useResolveEntries(): Record<ResolveKey, ResolveEntry> {
  return useResolveStore().entries
}

// useEscalation selects a single escalation entry from the store, keyed by
// anvil+beadID so same-id beads in different anvils stay distinct. Missing
// keys resolve to the shared IDLE_ESCALATION constant so consumers can
// destructure `status` without a null check on every render.
// eslint-disable-next-line react-refresh/only-export-components
export function useEscalation(escalationId: string, anvil?: string): EscalationEntry {
  const { escalations } = useResolveStore()
  return escalations[escalationKey(escalationId, anvil)] ?? IDLE_ESCALATION
}

// useEscalations exposes the full escalation map. Reserved for tests and
// bulk views; most callers should use useEscalation(id) instead.
// eslint-disable-next-line react-refresh/only-export-components
export function useEscalations(): Record<string, EscalationEntry> {
  return useResolveStore().escalations
}
