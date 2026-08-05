import { useCallback, useEffect, useRef, useState } from 'react'
import { useLocation } from 'react-router'
import { useAuth } from '../auth'

type StorageKind = 'session' | 'local'
type Scope = 'route' | 'user'

interface UseUIStateOptions {
  // storage selects sessionStorage (default) or localStorage. Session storage
  // is the right fit for transient pane state (sort orders, expanded rows);
  // local storage is for preferences that should survive a browser restart.
  storage?: StorageKind
  // scope decides how the key is namespaced. `route` partitions state by the
  // current pathname so two pages don't collide on the same logical key.
  // `user` partitions by authenticated user ID so per-user preferences don't
  // leak across accounts on a shared machine.
  scope?: Scope
}

export const KEY_PREFIX = 'forge.ui.'
const DEBOUNCE_MS = 150

function ssr(): boolean {
  return typeof window === 'undefined'
}

function getStore(kind: StorageKind): globalThis.Storage | null {
  if (ssr()) return null
  try {
    return kind === 'local' ? window.localStorage : window.sessionStorage
  } catch {
    // Storage access can throw in private-browsing or sandboxed iframes.
    return null
  }
}

function sanitize(seg: string): string {
  // Keep keys readable in DevTools — strip leading slashes and replace
  // characters that would make keys awkward to grep for.
  return seg.replace(/^\/+/, '').replace(/[^A-Za-z0-9_.\-]/g, '_') || 'root'
}

function buildKey(scopeID: string, key: string): string {
  return `${KEY_PREFIX}${sanitize(scopeID)}.${sanitize(key)}`
}

function readFromStore<T>(
  store: globalThis.Storage | null,
  key: string,
  fallback: T,
): T {
  if (!store) return fallback
  try {
    const raw = store.getItem(key)
    if (raw === null) return fallback
    return JSON.parse(raw) as T
  } catch {
    return fallback
  }
}

function writeToStore<T>(store: globalThis.Storage, key: string, value: T): void {
  try {
    store.setItem(key, JSON.stringify(value))
  } catch {
    // Quota-exceeded or serialization errors are non-fatal — we still keep
    // the in-memory value, just don't persist it.
  }
}

// useUIState is a typed sessionStorage/localStorage hook with debounced writes
// and SSR-safe initialization. It is intended as the single primitive for
// remembering pane-local UI state (sort orders, expanded rows, filter chips)
// so individual components opt in with a one-liner instead of reinventing
// storage glue.
export function useUIState<T>(
  key: string,
  initial: T,
  opts: UseUIStateOptions = {},
): [T, (value: T | ((prev: T) => T)) => void] {
  const { storage = 'session', scope = 'route' } = opts
  const location = useLocation()
  const { user } = useAuth()

  const scopeID = scope === 'route' ? location.pathname || 'default' : user || 'anon'
  const storageKey = buildKey(scopeID, key)
  const store = getStore(storage)

  const [value, setValue] = useState<T>(() => readFromStore(store, storageKey, initial))

  // stateRef mirrors `value` and serves as the source of truth for functional
  // updaters and the debounced writer. We update it directly inside the setter
  // so the timer callback doesn't depend on React's render cycle to observe
  // the latest value — important under fake timers and rapid sequences.
  const stateRef = useRef<T>(value)
  const storeRef = useRef(store)
  const keyRef = useRef(storageKey)
  const timerRef = useRef<number | undefined>(undefined)

  // Re-read when the storage key changes (route navigation, user login/logout)
  // or the storage backend swaps (rare, but supported via opts changes). Any
  // pending write is flushed to the previous key first so we don't lose
  // edits made just before navigating away.
  useEffect(() => {
    if (keyRef.current === storageKey && storeRef.current === store) return
    if (timerRef.current !== undefined) {
      window.clearTimeout(timerRef.current)
      if (storeRef.current) {
        writeToStore(storeRef.current, keyRef.current, stateRef.current)
      }
      timerRef.current = undefined
    }
    keyRef.current = storageKey
    storeRef.current = store
    const fresh = readFromStore(store, storageKey, initial)
    stateRef.current = fresh
    setValue(fresh)
    // `initial` is intentionally omitted from deps — consumers commonly pass
    // a fresh object/array literal each render, which would otherwise wipe
    // stored state on every parent re-render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [storageKey, store])

  const setter = useCallback((next: T | ((prev: T) => T)) => {
    const prev = stateRef.current
    const resolved =
      typeof next === 'function' ? (next as (p: T) => T)(prev) : next
    stateRef.current = resolved
    setValue(resolved)
    const target = storeRef.current
    if (!target) return
    if (timerRef.current !== undefined) {
      window.clearTimeout(timerRef.current)
    }
    timerRef.current = window.setTimeout(() => {
      writeToStore(target, keyRef.current, stateRef.current)
      timerRef.current = undefined
    }, DEBOUNCE_MS)
  }, [])

  // Flush on unmount so a quick edit + navigation doesn't drop the change.
  useEffect(() => {
    return () => {
      if (timerRef.current !== undefined) {
        window.clearTimeout(timerRef.current)
        timerRef.current = undefined
        if (storeRef.current) {
          writeToStore(storeRef.current, keyRef.current, stateRef.current)
        }
      }
    }
  }, [])

  return [value, setter]
}

// clearAll removes every forge.ui.* entry from both session and local storage.
// Exported primarily for tests; production code should not need to call this.
export function clearAll(prefix: string = KEY_PREFIX): void {
  if (ssr()) return
  for (const target of [getStore('session'), getStore('local')]) {
    if (!target) continue
    try {
      const toRemove: string[] = []
      for (let i = 0; i < target.length; i++) {
        const k = target.key(i)
        if (k && k.startsWith(prefix)) toRemove.push(k)
      }
      for (const k of toRemove) target.removeItem(k)
    } catch {
      // ignore
    }
  }
}
