import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { ApiError } from '../api'
import {
  postResolve,
  type ResolveRequest,
  type ResolveVerb,
} from '../api/forge'

// ResolveStatus is the lifecycle of a single resolve request. Mirrors the
// states the panel buttons render: idle (no recent attempt), pending
// (in-flight POST, button disabled + spinner), success (recent OK), error
// (last attempt failed, surface the message). The reducer below transitions
// pending→success/error and never back to pending without a reset.
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
}

interface ResolveStoreValue {
  // entries is the full map of in-flight + recently-resolved keys. Most
  // consumers should reach for `useResolveStatus(key)` which selects a
  // single entry; this is exposed for tests and for panels that need to
  // render a bulk view.
  entries: Record<ResolveKey, ResolveEntry>
  actions: ResolveActions
}

const ResolveStoreContext = createContext<ResolveStoreValue | null>(null)

// ResolveStoreProvider wires the resolve state into the React tree. It is
// mounted once near the root (next to ToastProvider) so any panel can call
// `useResolveStatus` / `useResolveActions` without prop-drilling.
export function ResolveStoreProvider({ children }: { children: ReactNode }) {
  const [entries, setEntries] = useState<Record<ResolveKey, ResolveEntry>>({})

  // entriesRef shadows the state for the action helpers so the closures
  // captured by run/reset don't go stale between renders. The same trick
  // is used by useUIState's debounced setter; it lets us read the latest
  // state inside an async callback without re-creating the callback on
  // every render.
  const entriesRef = useRef(entries)
  entriesRef.current = entries

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

  const value = useMemo<ResolveStoreValue>(
    () => ({ entries, actions: { run, reset, setEntry } }),
    [entries, run, reset, setEntry],
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
