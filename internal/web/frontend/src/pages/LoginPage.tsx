import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { Hammer, Loader2 } from 'lucide-react'
import { useAuth } from '../auth'

export default function LoginPage() {
  const { authenticated, loading, login } = useAuth()
  const navigate = useNavigate()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!loading && authenticated) {
      navigate('/', { replace: true })
    }
  }, [authenticated, loading, navigate])

  // Don't flash the login form while the auth probe is in flight or while
  // a redirect to the dashboard is pending — show a spinner instead.
  if (loading || authenticated) {
    return (
      <div className="flex h-full items-center justify-center">
        <div
          className="h-8 w-8 animate-spin rounded-full border-2 border-slate-700 border-t-amber-400"
          role="status"
          aria-live="polite"
          aria-label="Loading"
        />
      </div>
    )
  }

  async function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    if (submitting) return
    setSubmitting(true)
    setError(null)
    const result = await login(username, password)
    if (!result.ok) {
      setError(result.error ?? 'login failed')
      setSubmitting(false)
      return
    }
    navigate('/', { replace: true })
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-10">
      <div className="w-full max-w-sm rounded-2xl border border-slate-800 bg-slate-900/60 p-6 shadow-xl backdrop-blur">
        <div className="mb-6 flex items-center gap-3">
          <Hammer size={28} className="text-amber-400" aria-hidden />
          <div>
            <h1 className="text-xl font-semibold text-slate-100">Hearth</h1>
            <p className="text-xs text-slate-400">Forge orchestrator dashboard</p>
          </div>
        </div>

        <form onSubmit={onSubmit} className="flex flex-col gap-4">
          <label className="flex flex-col gap-1.5 text-sm text-slate-300">
            <span className="font-medium">Username</span>
            <input
              type="text"
              autoComplete="username"
              autoFocus
              required
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              className="rounded-lg border border-slate-700 bg-slate-950 px-3 py-2 text-slate-100 placeholder:text-slate-500 focus:border-amber-400 focus:outline-none"
              placeholder="alice"
            />
          </label>

          <label className="flex flex-col gap-1.5 text-sm text-slate-300">
            <span className="font-medium">Password</span>
            <input
              type="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="rounded-lg border border-slate-700 bg-slate-950 px-3 py-2 text-slate-100 placeholder:text-slate-500 focus:border-amber-400 focus:outline-none"
              placeholder="••••••••"
            />
          </label>

          {error && (
            <div
              role="alert"
              className="rounded-md border border-red-700/60 bg-red-900/30 px-3 py-2 text-sm text-red-200"
            >
              {error}
            </div>
          )}

          <button
            type="submit"
            disabled={submitting}
            className="mt-2 inline-flex items-center justify-center gap-2 rounded-lg bg-amber-500 px-4 py-2 text-sm font-semibold text-slate-950 transition-colors hover:bg-amber-400 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {submitting ? <Loader2 size={16} className="animate-spin" /> : null}
            {submitting ? 'Signing in…' : 'Sign in'}
          </button>
        </form>

        <p className="mt-6 text-center text-xs text-slate-500">
          Sessions expire after a period of inactivity and are capped at an
          absolute lifetime. Configure users via{' '}
          <code className="text-slate-400">FORGE_USERS</code>.
        </p>
      </div>
    </div>
  )
}
