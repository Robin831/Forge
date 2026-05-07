import { useEffect, useRef, useState } from 'react'
import { ApiError, apiGet } from '../api'
import { useAuth } from '../auth'

interface PollState<T> {
  data: T | null
  error: string | null
  loading: boolean
}

// useApiPoll polls the given path on a fixed interval and exposes
// {data, error, loading}. On 401 it triggers logout via the auth context so
// the user is redirected to /login. Polling pauses when the document is
// hidden to avoid burning daemon CPU on a backgrounded tab.
export function useApiPoll<T>(path: string, intervalMs = 5000): PollState<T> {
  const { logout } = useAuth()
  const [state, setState] = useState<PollState<T>>({
    data: null,
    error: null,
    loading: true,
  })
  // eslint-disable-next-line react-hooks/refs
  const mountedRef = useRef(true)

  useEffect(() => {
    mountedRef.current = true
    let cancelled = false
    let timer: number | undefined

    const tick = async () => {
      if (cancelled) return
      try {
        const data = await apiGet<T>(path)
        if (cancelled || !mountedRef.current) return
        setState({ data, error: null, loading: false })
      } catch (err) {
        if (cancelled || !mountedRef.current) return
        if (err instanceof ApiError && err.status === 401) {
          void logout()
          return
        }
        const message = err instanceof Error ? err.message : 'request failed'
        setState((prev) => ({ data: prev.data, error: message, loading: false }))
      } finally {
        if (!cancelled && mountedRef.current) {
          timer = window.setTimeout(tick, intervalMs)
        }
      }
    }

    void tick()

    const onVisibility = () => {
      if (document.visibilityState === 'visible' && !cancelled) {
        if (timer !== undefined) {
          window.clearTimeout(timer)
        }
        void tick()
      }
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      cancelled = true
      mountedRef.current = false
      if (timer !== undefined) {
        window.clearTimeout(timer)
      }
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [path, intervalMs, logout])

  return state
}
