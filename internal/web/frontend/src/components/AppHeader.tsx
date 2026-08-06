import { Activity, Coins, FlaskConical, GitPullRequest, Hammer, History, LayoutDashboard, LogOut, MonitorPlay, Package, Settings, Sparkles } from 'lucide-react'
import { NavLink } from 'react-router'
import { useAuth } from '../auth'
import { usePreviewsList } from '../hooks/usePreview'

interface NavItem {
  to: string
  label: string
  icon: typeof LayoutDashboard
  end?: boolean
}

const navItems: NavItem[] = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard, end: true },
  { to: '/ingots', label: 'Ingots', icon: Package },
  { to: '/crucibles', label: 'Crucibles', icon: FlaskConical },
  { to: '/history', label: 'History', icon: History },
  { to: '/prs', label: 'PRs', icon: GitPullRequest },
  { to: '/forge', label: 'Forge', icon: Sparkles },
  { to: '/costs', label: 'Costs', icon: Coins },
  { to: '/settings', label: 'Settings', icon: Settings },
]

// The Previews tab is only meaningful when the daemon runs a Kiln manager, so
// it is appended from the shared previews snapshot rather than hard-coded. The
// snapshot is already polled by every preview button on the page, so gating the
// nav on it costs nothing extra.
const previewsNavItem: NavItem = { to: '/previews', label: 'Previews', icon: MonitorPlay }

interface AppHeaderProps {
  daemonOnline?: boolean
  daemonLoading?: boolean
}

export default function AppHeader({ daemonOnline, daemonLoading }: AppHeaderProps) {
  const { user, logout } = useAuth()
  const previews = usePreviewsList()
  const items = previews.enabled ? [...navItems, previewsNavItem] : navItems

  return (
    <header className="flex flex-wrap items-center gap-3">
      <div className="flex items-center gap-3">
        <Hammer size={24} className="text-amber-400" aria-hidden />
        <div>
          <h1 className="text-xl font-semibold text-slate-100 sm:text-2xl">Hearth</h1>
          <p className="text-xs text-slate-400">Forge orchestrator dashboard</p>
        </div>
      </div>

      <nav
        aria-label="Primary"
        className="ml-2 flex flex-wrap items-center gap-1 rounded-lg border border-slate-800 bg-slate-900/60 p-1 text-sm"
      >
        {items.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) =>
              `inline-flex items-center gap-1.5 rounded-md px-2.5 py-1.5 transition-colors ${
                isActive
                  ? 'bg-amber-400/15 text-amber-200'
                  : 'text-slate-300 hover:bg-slate-800/60 hover:text-slate-100'
              }`
            }
          >
            <Icon size={14} aria-hidden />
            <span>{label}</span>
          </NavLink>
        ))}
      </nav>

      {typeof daemonOnline === 'boolean' && (
        <span
          className={`ml-auto inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs ${
            daemonOnline
              ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300'
              : daemonLoading
                ? 'border-slate-700 bg-slate-800/60 text-slate-400'
                : 'border-red-500/40 bg-red-500/10 text-red-300'
          }`}
          aria-live="polite"
        >
          <Activity size={10} aria-hidden />
          {daemonLoading
            ? 'connecting…'
            : daemonOnline
              ? 'daemon online'
              : 'daemon offline'}
        </span>
      )}

      {user && (
        <span className="hidden items-center text-sm text-slate-400 sm:inline-flex">{user}</span>
      )}

      <button
        type="button"
        onClick={() => void logout()}
        className="inline-flex items-center gap-1.5 rounded-lg border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm font-medium text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700"
      >
        <LogOut size={14} aria-hidden />
        <span>Sign out</span>
      </button>
    </header>
  )
}
