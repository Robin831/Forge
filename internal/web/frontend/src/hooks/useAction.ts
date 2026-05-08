import { useCallback, useState } from 'react'
import { ApiError } from '../api'
import { useToast } from './useToast'

interface RunActionOptions {
  // Message to show on a successful 200/202 response.
  successMessage: string
  // Optional handler invoked after the action resolves successfully — useful
  // for triggering an immediate poll refresh on the calling page.
  onSuccess?: () => void
}

interface UseActionResult {
  // run executes the action. The promise resolves to true on success,
  // false on failure. Errors are surfaced via toast automatically.
  run: <T>(fn: () => Promise<T>, opts: RunActionOptions) => Promise<boolean>
  busy: boolean
}

// useAction wraps an async API call with a toast + busy flag. It is shared
// between the queue pane, workers pane and bead detail page so the UX stays
// consistent: optimistic UI, success toast, and an error toast carrying the
// daemon's message when something fails.
export function useAction(): UseActionResult {
  const toast = useToast()
  const [busy, setBusy] = useState(false)

  const run = useCallback(
    async <T,>(fn: () => Promise<T>, opts: RunActionOptions): Promise<boolean> => {
      setBusy(true)
      try {
        await fn()
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
