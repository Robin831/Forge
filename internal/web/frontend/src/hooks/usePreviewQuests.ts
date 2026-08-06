// usePreviewQuests — client state for running a bead's E2E quests against its
// live Kiln preview.
//
// It is deliberately separate from usePreview. A preview is a long-lived
// environment every dense surface in the dashboard reads about (hence
// usePreview's one shared, always-on poll); a quest run is an occasional,
// per-bead action watched by exactly one open panel. Folding it into the shared
// store would make every PR row poll for runs nobody asked for.
//
// Nothing here gates anything. A failed run is a signal to the human reading
// the panel — no merge, PR or pipeline decision reads a run's status — so the
// hook never surfaces a "blocked" notion and the panel styles failures as
// warnings.

import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api'
import { fetchQuestRun, startQuestRun, type QuestRunSummary } from '../api/previews'

// Cadence while a run is in flight. Quests take minutes and each poll is a
// small JSON read, so this is a compromise between a responsive panel and not
// hammering the daemon while a browser is busy.
const RUNNING_POLL_INTERVAL_MS = 3000

export interface UsePreviewQuestsOptions {
  /**
   * Whether the action is offered at all: the anvil opted into preview quests
   * AND the preview is healthy. A disabled hook does not poll — a bead whose
   * anvil never opted in should cost nothing.
   */
  enabled: boolean
  /** Called with a human-readable message when a dispatch fails. */
  onError?: (message: string) => void
  /** Called once the daemon has accepted a run. */
  onStarted?: (runID: string) => void
}

export interface UsePreviewQuestsResult {
  /** The latest run for this bead, or null when it has never had one. */
  run: QuestRunSummary | null
  /** A run is in flight (dispatching, or the daemon reports it running). */
  isRunning: boolean
  /** The last dispatch failure, cleared when a new run is dispatched. */
  error: string | null
  /** The first load has completed (or was skipped because it is disabled). */
  loaded: boolean
  start: () => Promise<void>
  refresh: () => Promise<void>
}

export function usePreviewQuests(
  beadID: string,
  options: UsePreviewQuestsOptions,
): UsePreviewQuestsResult {
  const { enabled } = options
  const [run, setRun] = useState<QuestRunSummary | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loaded, setLoaded] = useState(false)
  const [dispatching, setDispatching] = useState(false)

  // Callbacks read through a ref so inline arrows from the caller do not
  // re-create start() (and restart the poll effect) on every render.
  const callbacksRef = useRef(options)
  useEffect(() => {
    callbacksRef.current = options
  })

  // Synchronous double-click guard: two clicks in one tick both read the same
  // (still false) `dispatching` state.
  const busyRef = useRef(false)

  const refresh = useCallback(async () => {
    try {
      const res = await fetchQuestRun(beadID)
      setRun(res.found && res.run ? res.run : null)
    } catch {
      // A failed poll keeps the last known run rather than blanking the panel:
      // the run itself is unaffected by our inability to read it.
    } finally {
      setLoaded(true)
    }
  }, [beadID])

  // Load once when the action becomes available, so a panel opened after a run
  // finished still shows it.
  useEffect(() => {
    if (!enabled) {
      setLoaded(false)
      return
    }
    let cancelled = false
    void (async () => {
      const res = await fetchQuestRun(beadID).catch(() => null)
      if (cancelled) return
      setRun(res?.found && res.run ? res.run : null)
      setLoaded(true)
    })()
    return () => {
      cancelled = true
    }
  }, [beadID, enabled])

  const isRunning = dispatching || run?.status === 'running'

  // Poll only while something is actually running. A finished run is immutable,
  // so there is nothing to poll for once it lands.
  useEffect(() => {
    if (!enabled || !isRunning) return
    const timer = setInterval(() => {
      void refresh()
    }, RUNNING_POLL_INTERVAL_MS)
    return () => clearInterval(timer)
  }, [enabled, isRunning, refresh])

  const start = useCallback(async () => {
    if (busyRef.current) return
    busyRef.current = true
    setDispatching(true)
    setError(null)
    try {
      const res = await startQuestRun(beadID)
      if (res.run) setRun(res.run)
      callbacksRef.current.onStarted?.(res.run_id)
      // The 202 body already carries the freshly-created run; refresh anyway so
      // a run that finished between dispatch and now is not shown as running.
      await refresh()
    } catch (err) {
      const message =
        err instanceof ApiError || err instanceof Error ? err.message : 'failed to run quests'
      setError(message)
      callbacksRef.current.onError?.(message)
    } finally {
      busyRef.current = false
      setDispatching(false)
    }
  }, [beadID, refresh])

  return { run, isRunning, error, loaded, start, refresh }
}
