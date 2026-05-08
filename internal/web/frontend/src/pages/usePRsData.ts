// usePRsData backs the /prs tab. It uses a module-level cache so multiple
// subscribers issue at most one fetch per polling window — the cache is
// daemon-scoped (a different daemon would be on a different origin), which
// prevents cross-repo bleed. Callers that need only one section should
// destructure the return value rather than wrapping this hook, to avoid
// duplicate interval and visibilitychange listener registrations.
//
// Cache contract (per Forge-9ye8):
//   - 60-second TTL: stale entries trigger a background refetch on the
//     next hook mount or visibility-change.
//   - manual invalidation: refresh() clears the timestamp and forces a
//     refetch immediately, even if the entry is fresh.
//   - loading reflects an in-flight request, not "data is null". On a
//     manual refresh, prior data stays visible while loading is true.

import { useCallback, useEffect, useState } from 'react'
import { ApiError, apiGet, type PRItem, type PRsResponse } from '../api'
import { useAuth } from '../auth'

export const PRS_API_PATH = '/api/prs/all'
export const PRS_CACHE_TTL_MS = 60_000

interface CacheEntry {
  data: PRsResponse | null
  fetchedAt: number
  error: string | null
}

const emptyResponse: PRsResponse = {
  forge_prs: [],
  external_prs: [],
  recently_merged: [],
}

let cache: CacheEntry = {
  data: null,
  fetchedAt: 0,
  error: null,
}

let inflight: Promise<void> | null = null
const subscribers = new Set<() => void>()

function notify() {
  for (const fn of subscribers) fn()
}

function isStale(): boolean {
  if (cache.fetchedAt === 0) return true
  return Date.now() - cache.fetchedAt >= PRS_CACHE_TTL_MS
}

async function performFetch(onUnauthorized: () => void): Promise<void> {
  if (inflight) return inflight
  inflight = (async () => {
    notify()
    try {
      const data = await apiGet<PRsResponse>(PRS_API_PATH)
      cache = {
        data,
        fetchedAt: Date.now(),
        error: null,
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onUnauthorized()
        return
      }
      const message = err instanceof Error ? err.message : 'request failed'
      cache = {
        data: cache.data,
        fetchedAt: Date.now(),
        error: message,
      }
    } finally {
      inflight = null
      notify()
    }
  })()
  return inflight
}

export interface PRsDataState {
  forge_prs: PRItem[]
  external_prs: PRItem[]
  recently_merged: PRItem[]
  loading: boolean
  error: string | null
  fetchedAt: number | null
  refresh: () => void
}

// usePRsData returns all three PR sections plus loading/error state and a
// refresh function that bypasses the in-memory cache. Render this once at
// the top of a page and pass slices down, or use the per-section convenience
// hooks below.
export function usePRsData(): PRsDataState {
  const { logout } = useAuth()
  const [, setTick] = useState(0)

  const onUnauthorized = useCallback(() => {
    void logout()
  }, [logout])

  useEffect(() => {
    const sub = () => setTick((n) => n + 1)
    subscribers.add(sub)

    if (isStale()) {
      void performFetch(onUnauthorized)
    }

    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible' && isStale()) {
        void performFetch(onUnauthorized)
      }
    }, PRS_CACHE_TTL_MS)

    const onVisibility = () => {
      if (document.visibilityState === 'visible' && isStale()) {
        void performFetch(onUnauthorized)
      }
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      subscribers.delete(sub)
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [onUnauthorized])

  const refresh = useCallback(() => {
    cache = { ...cache, fetchedAt: 0, error: null }
    void performFetch(onUnauthorized)
  }, [onUnauthorized])

  const data = cache.data ?? emptyResponse
  return {
    forge_prs: data.forge_prs,
    external_prs: data.external_prs,
    recently_merged: data.recently_merged,
    loading: inflight !== null,
    error: cache.error,
    fetchedAt: cache.fetchedAt > 0 ? cache.fetchedAt : null,
    refresh,
  }
}

