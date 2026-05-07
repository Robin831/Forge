import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'

interface AuthState {
  authenticated: boolean
  user: string | null
  loading: boolean
  login: (user: string, password: string) => Promise<{ ok: boolean; error?: string }>
  logout: () => Promise<void>
  refresh: () => Promise<void>
}

const AuthContext = createContext<AuthState>({
  authenticated: false,
  user: null,
  loading: true,
  login: async () => ({ ok: false, error: 'auth context not initialised' }),
  logout: async () => {},
  refresh: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [authenticated, setAuthenticated] = useState(false)
  const [user, setUser] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const refresh = useCallback(async () => {
    try {
      const res = await fetch('/login', { credentials: 'include' })
      if (!res.ok) {
        setAuthenticated(false)
        setUser(null)
        return
      }
      const data = (await res.json()) as { authenticated?: boolean; user?: string }
      setAuthenticated(!!data.authenticated)
      setUser(data.user ?? null)
    } catch {
      setAuthenticated(false)
      setUser(null)
    }
  }, [])

  useEffect(() => {
    void refresh().finally(() => setLoading(false))
  }, [refresh])

  const login = useCallback(
    async (username: string, password: string) => {
      const body = new URLSearchParams({ user: username, password })
      try {
        const res = await fetch('/login', {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: body.toString(),
        })
        if (!res.ok) {
          const data = (await res.json().catch(() => ({}))) as { error?: string }
          return { ok: false, error: data.error ?? `HTTP ${res.status}` }
        }
        const data = (await res.json()) as { authenticated?: boolean; user?: string }
        setAuthenticated(!!data.authenticated)
        setUser(data.user ?? username)
        return { ok: true }
      } catch (err) {
        return { ok: false, error: err instanceof Error ? err.message : 'Network error' }
      }
    },
    [],
  )

  const logout = useCallback(async () => {
    try {
      await fetch('/logout', { method: 'POST', credentials: 'include' })
    } catch {
      // Ignore — clearing local state below is the important part.
    }
    setAuthenticated(false)
    setUser(null)
  }, [])

  const value = useMemo(
    () => ({ authenticated, user, loading, login, logout, refresh }),
    [authenticated, user, loading, login, logout, refresh],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

// eslint-disable-next-line react-refresh/only-export-components
export function useAuth() {
  return useContext(AuthContext)
}
