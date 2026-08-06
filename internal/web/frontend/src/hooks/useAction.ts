import { useCallback, useState } from 'react'
import { ApiError, isUnresolvedQueued } from '../api'
import { useToast } from './useToast'

interface RunActionOptions {
  // Message to show once the action is confirmed to have succeeded.
  successMessage: string
  // Optional handler invoked after the action resolves successfully — useful
  // for triggering an immediate poll refresh on the calling page.
  onSuccess?: () => void
}

interface UseActionResult {
  // run executes the action. The promise resolves to true on a confirmed
  // success, false on failure or an unconfirmed outcome. Errors are surfaced
  // via toast automatically.
  run: <T>(fn: () => Promise<T>, opts: RunActionOptions) => Promise<boolean>
  busy: boolean
}

// useAction wraps an async API call with a toast + busy flag. It is shared
// between the queue pane, workers pane and bead detail page so the UX stays
// consistent: optimistic UI, success toast, and an error toast carrying the
// daemon's message when something fails.
//
// Asynchronous ("queued") actions are resolved by apiPost before we get here:
// a queued command that fails throws, so it lands in the error branch. A
// queued command whose outcome could not be determined comes back tagged
// `queued_unresolved` and is reported neutrally — never as success, since the
// write may have been silently discarded (Forge-4r2n).
export function useAction(): UseActionResult {
  const toast = useToast()
  const [busy, setBusy] = useState(false)

  const run = useCallback(
    async <T,>(fn: () => Promise<T>, opts: RunActionOptions): Promise<boolean> => {
      setBusy(true)
      try {
        const result = await fn()
        if (isUnresolvedQueued(result)) {
          toast.push(`Queued, outcome unknown: ${opts.successMessage}`, 'info')
          // Still refresh: the command may yet land, and the next poll is the
          // only thing that will show it.
          opts.onSuccess?.()
          return false
        }
        toast.push(opts.successMessage, 'success')
        opts.onSuccess?.()
        return true
      } catch (err) {
        const message =
          err instanceof ApiError
            ? err.message
            : err instanceof Error
              ? err.message
              : 'request failed'
        toast.push(message, 'error')
        return false
      } finally {
        setBusy(false)
      }
    },
    [toast],
  )

  return { run, busy }
}
