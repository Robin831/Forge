// usePreview — the shared client state machine for one bead's Kiln preview.
//
// This module is the single place the SPA models "what is this bead's preview
// doing": the trigger button, the status chip, the bead-detail panel and the
// previews overview all read from it, so none of them can disagree about what
// `starting` means.
//
// Two things make it more than a fetch wrapper:
//
//  1. A start has no row to poll. kiln.Manager publishes a preview into its
//     registry only once every service is up and healthy, so for the whole
//     start — worktree, setup script, spawn, health checks — GET /api/previews
//     reports nothing at all. The authoritative in-flight signal is the queued
//     request's poll_url; the list takes over once the row appears.
//  2. Every mounted consumer would otherwise poll the same global list. The
//     previews snapshot therefore lives in one module-level store that polls
//     once for all subscribers, so a page of twenty PR rows costs one request.

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { ApiError, fetchRequestStatus } from '../api'
import {
  fetchPreviews,
  mapPreviewStatus,
  pollURLFor,
  previewErrorText,
  startPreview,
  stopPreview,
  type PreviewStatus,
  type PreviewSummary,
} from '../api/previews'

// Background cadence for the shared previews list. Matches the dashboard's
// other panes; the list is cheap and previews are long-lived.
const PREVIEWS_POLL_INTERVAL_MS = 5000

// Cadence while this bead has a start/stop in flight. Faster than the
// background poll because the operator is watching a spinner.
const ACTIVE_POLL_INTERVAL_MS = 1500

// Backstop for a start that never resolves. The daemon bounds one start at 15
// minutes (previewStartTimeout) and reports its own timeout as a request
// error, so this only fires when the daemon stops answering at all — hence the
// deliberate slack past the daemon's own bound.
const START_TIMEOUT_MS = 16 * 60_000

// Backstop for a teardown. The daemon bounds one at 2 minutes.
const STOP_TIMEOUT_MS = 3 * 60_000

// Grace period after the request record reads `unknown` — the daemon no longer
// holds an outcome for it, which in practice means it restarted mid-command.
// We keep watching the list briefly in case the preview did come up, then give
// up rather than spin until the full timeout.
const UNKNOWN_GRACE_MS = 60_000

// --- shared previews store -------------------------------------------------

// PreviewsSnapshot is the store's immutable view of GET /api/previews. It is
// replaced wholesale on every change so useSyncExternalStore can compare by
// identity.
export interface PreviewsSnapshot {
  enabled: boolean
  anvils: string[]
  /** Anvils whose quests may be run against a preview (`preview_quests`). */
  quest_anvils: string[]
  previews: PreviewSummary[]
  /** false until the first response lands — gates on it read as "not yet". */
  loaded: boolean
  error: string | null
  /**
   * When these previews were fetched (epoch ms); 0 before the first response.
   * It is the anchor the idle countdown decrements from, so it stays pinned to
   * the last *successful* fetch — a failed poll keeps serving the previews it
   * came with, and their countdown must keep running from when they were read.
   */
  fetchedAt: number
}

const EMPTY_SNAPSHOT: PreviewsSnapshot = {
  enabled: false,
  anvils: [],
  quest_anvils: [],
  previews: [],
  loaded: false,
  error: null,
  fetchedAt: 0,
}

let snapshot: PreviewsSnapshot = EMPTY_SNAPSHOT
const listeners = new Set<() => void>()
let pollTimer: ReturnType<typeof setTimeout> | undefined
let inFlight: Promise<void> | null = null

function publish(next: PreviewsSnapshot): void {
  snapshot = next
  for (const listener of [...listeners]) listener()
}

// getPreviewsSnapshot returns the current store value. Stable by identity
// between changes, which is what useSyncExternalStore requires.
export function getPreviewsSnapshot(): PreviewsSnapshot {
  return snapshot
}

// refreshPreviews fetches the list once, collapsing concurrent callers onto a
// single request. A failure keeps the last-known previews and records the
// message rather than blanking the UI.
export function refreshPreviews(): Promise<void> {
  if (inFlight) return inFlight
  inFlight = (async () => {
    try {
      const data = await fetchPreviews()
      publish({
        enabled: data.enabled,
        anvils: data.anvils,
        quest_anvils: data.quest_anvils,
        previews: data.previews,
        loaded: true,
        error: null,
        fetchedAt: Date.now(),
      })
    } catch (err) {
      const message = err instanceof Error ? err.message : 'failed to load previews'
      publish({ ...snapshot, loaded: true, error: message })
    } finally {
      inFlight = null
    }
  })()
  return inFlight
}

function schedulePoll(): void {
  if (pollTimer !== undefined || listeners.size === 0) return
  pollTimer = setTimeout(() => {
    pollTimer = undefined
    if (listeners.size === 0) return
    void refreshPreviews().finally(schedulePoll)
  }, PREVIEWS_POLL_INTERVAL_MS)
}

// subscribePreviews registers a listener and keeps the shared poll running for
// as long as anyone is listening. The first subscriber triggers an immediate
// fetch; later ones join the poll already in progress rather than firing their
// own request.
export function subscribePreviews(listener: () => void): () => void {
  listeners.add(listener)
  if (pollTimer === undefined) {
    void refreshPreviews().finally(schedulePoll)
  }
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0 && pollTimer !== undefined) {
      clearTimeout(pollTimer)
      pollTimer = undefined
    }
  }
}

// resetPreviewsStore drops all state and timers. Exported for tests, which
// otherwise leak a snapshot from one case into the next.
export function resetPreviewsStore(): void {
  listeners.clear()
  if (pollTimer !== undefined) {
    clearTimeout(pollTimer)
    pollTimer = undefined
  }
  inFlight = null
  snapshot = EMPTY_SNAPSHOT
}

// usePreviewsList subscribes to the shared snapshot without singling out a
// bead. The previews overview page and the nav gate read every live preview at
// once, and they join the same poll the per-bead consumers already run rather
// than opening a second one.
export function usePreviewsList(): PreviewsSnapshot {
  return useSyncExternalStore(subscribePreviews, getPreviewsSnapshot, getPreviewsSnapshot)
}

// --- the hook --------------------------------------------------------------

// PendingAction is an in-flight start/stop: which verb, where to resolve its
// outcome (null once resolved or never queued), and when to give up.
interface PendingAction {
  kind: 'start' | 'stop'
  pollUrl: string | null
  deadline: number
}

export interface UsePreviewOptions {
  /** Owning anvil. Required to start — the daemon reads the manifest from it. */
  anvil?: string
  /**
   * Branch to preview. Left unset for every bead-owned preview, where the
   * daemon's own default — the bead's forge/<bead-id> branch — is the right
   * answer. Only the ad-hoc form on the previews page names one, since there
   * the bead id is a registry key rather than a branch it can be derived from.
   */
  branch?: string
  /** Called with a human-readable message when a start/stop fails. */
  onError?: (message: string) => void
  /** Called once the daemon has accepted a start/stop. */
  onQueued?: (kind: 'start' | 'stop') => void
}

export interface UsePreviewResult {
  /** Kiln is running in the daemon at all. */
  enabled: boolean
  /** A preview can be started for this bead: Kiln is on and its anvil has a manifest. */
  available: boolean
  status: PreviewStatus
  /** The live preview record, or null when this bead has none. */
  preview: PreviewSummary | null
  /**
   * When the snapshot `preview` came from was fetched (epoch ms); 0 before the
   * first response. Callers rendering the idle countdown need it to age the
   * daemon's seconds-remaining between polls.
   */
  fetchedAt: number
  /** The entry URL to open, or null when there is nothing to open yet. */
  previewUrl: string | null
  /** Why the preview failed, when it did. */
  error: string | null
  /** A start or stop is in flight. */
  isBusy: boolean
  start: () => Promise<void>
  stop: () => Promise<void>
  refresh: () => Promise<void>
}

export function usePreview(beadID: string, options: UsePreviewOptions = {}): UsePreviewResult {
  const { anvil, branch } = options
  const snap = useSyncExternalStore(subscribePreviews, getPreviewsSnapshot, getPreviewsSnapshot)

  // Callbacks are read through a ref so a caller passing inline arrows does not
  // re-create start/stop (and restart the poll effect) on every render.
  const callbacksRef = useRef(options)
  useEffect(() => {
    callbacksRef.current = options
  })

  const [pending, setPending] = useState<PendingAction | null>(null)
  const [failure, setFailure] = useState<string | null>(null)

  // Synchronous mirror of `pending` for the start/stop guards: two clicks in
  // one tick both read the same (still null) `pending` state, so the state
  // alone cannot stop a double dispatch.
  const busyRef = useRef(false)
  useEffect(() => {
    if (!pending) busyRef.current = false
  }, [pending])

  const preview = useMemo(
    () => snap.previews.find((p) => p.bead_id === beadID) ?? null,
    [snap.previews, beadID],
  )

  const fail = useCallback((message: string) => {
    setPending(null)
    setFailure(message)
    callbacksRef.current.onError?.(message)
  }, [])

  // Resolve an in-flight action: poll the queued request for its outcome and
  // the list for the row it produces. Runs only while something is pending.
  useEffect(() => {
    if (!pending) return
    let cancelled = false

    const tick = async () => {
      if (cancelled) return
      if (Date.now() >= pending.deadline) {
        fail(
          pending.kind === 'start'
            ? 'Timed out waiting for the preview to start.'
            : 'Timed out waiting for the preview to stop.',
        )
        return
      }
      await refreshPreviews()
      if (cancelled || !pending.pollUrl) return

      const outcome = await fetchRequestStatus(pending.pollUrl)
      if (cancelled) return
      if (outcome.state === 'error') {
        fail(outcome.message || `the preview ${pending.kind} command failed`)
        void refreshPreviews()
        return
      }
      if (outcome.state === 'ok') {
        // The command landed; the list is now the source of truth. Stop asking
        // about the request and let the row (or its absence) settle the state.
        setPending((prev) => (prev ? { ...prev, pollUrl: null } : prev))
        void refreshPreviews()
        return
      }
      if (outcome.state === 'unknown') {
        // The daemon has no record of the request — almost always a restart
        // mid-command. Watch the list a little longer, then give up.
        setPending((prev) =>
          prev
            ? {
                ...prev,
                pollUrl: null,
                deadline: Math.min(prev.deadline, Date.now() + UNKNOWN_GRACE_MS),
              }
            : prev,
        )
      }
    }

    void tick()
    const timer = setInterval(() => {
      void tick()
    }, ACTIVE_POLL_INTERVAL_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [pending, fail])

  // Retire a pending action once the list reflects it: a start is done when the
  // bead has a settled row, a stop when the row is gone.
  useEffect(() => {
    if (!pending) return
    if (pending.kind === 'stop') {
      if (!preview) setPending(null)
      return
    }
    if (!preview) return
    const remote = mapPreviewStatus(preview.status)
    if (remote === 'healthy' || remote === 'degraded' || remote === 'failed') {
      setPending(null)
    }
  }, [pending, preview])

  const start = useCallback(async () => {
    if (pending || busyRef.current) return
    if (!anvil) {
      const message = 'Cannot start a preview without an anvil.'
      setFailure(message)
      callbacksRef.current.onError?.(message)
      return
    }
    busyRef.current = true
    setFailure(null)
    setPending({ kind: 'start', pollUrl: null, deadline: Date.now() + START_TIMEOUT_MS })
    try {
      const queued = await startPreview(beadID, anvil, branch)
      const url = pollURLFor(queued)
      setPending((prev) => (prev && prev.kind === 'start' ? { ...prev, pollUrl: url } : prev))
      callbacksRef.current.onQueued?.('start')
    } catch (err) {
      fail(err instanceof ApiError || err instanceof Error ? err.message : 'failed to start preview')
    }
  }, [anvil, beadID, branch, fail, pending])

  const stop = useCallback(async () => {
    if (pending || busyRef.current) return
    busyRef.current = true
    setFailure(null)
    setPending({ kind: 'stop', pollUrl: null, deadline: Date.now() + STOP_TIMEOUT_MS })
    try {
      const queued = await stopPreview(beadID, anvil)
      const url = pollURLFor(queued)
      setPending((prev) => (prev && prev.kind === 'stop' ? { ...prev, pollUrl: url } : prev))
      callbacksRef.current.onQueued?.('stop')
    } catch (err) {
      fail(err instanceof ApiError || err instanceof Error ? err.message : 'failed to stop preview')
    }
  }, [anvil, beadID, fail, pending])

  // The recorded status, folded with whatever is in flight locally. A pending
  // verb wins over "no row yet" but never over a row the daemon has published:
  // if the list says failed while we still think we are starting, it failed.
  const remote = preview ? mapPreviewStatus(preview.status) : 'idle'
  let status: PreviewStatus
  if (pending?.kind === 'start') {
    status = remote === 'idle' ? 'starting' : remote
  } else if (pending?.kind === 'stop') {
    status = remote === 'idle' ? 'idle' : 'stopping'
  } else if (failure && !preview) {
    status = 'failed'
  } else {
    status = remote
  }

  return {
    enabled: snap.enabled,
    available: snap.enabled && !!anvil && snap.anvils.includes(anvil),
    status,
    preview,
    fetchedAt: snap.fetchedAt,
    previewUrl: preview?.entry_url ? preview.entry_url : null,
    error: failure ?? previewErrorText(preview),
    isBusy: pending !== null,
    start,
    stop,
    refresh: refreshPreviews,
  }
}
