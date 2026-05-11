import { Coins } from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import type { CostsResponse, StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import Pane, { EmptyState } from '../components/Pane'

const POLL_INTERVAL_MS = 30000

function formatUSD(n: number): string {
  if (!isFinite(n)) return '$0.00'
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 4,
  }).format(n)
}

function formatTokens(n: number): string {
  if (n < 1000) return n.toLocaleString()
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}K`
  return `${(n / 1_000_000).toFixed(2)}M`
}

export default function CostsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const costs = useApiPoll<CostsResponse>('/api/costs?days=14', POLL_INTERVAL_MS)

  const today = costs.data?.today
  const limit = costs.data?.today_limit ?? 0
  const recent = costs.data?.recent ?? []
  const providers = costs.data?.today_providers ?? []
  const dailyLimit = status.data?.daily_cost_limit ?? limit
  const limitReached = !!status.data?.cost_limit_paused

  const limitPct = dailyLimit > 0 && today ? Math.min(100, (today.estimated_cost / dailyLimit) * 100) : 0

  return (
    <div className="flex min-h-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <section
        aria-label="Today's spend"
        className="grid grid-cols-1 gap-4 rounded-xl border border-slate-800 bg-slate-900/60 p-4 sm:grid-cols-3"
      >
        <div>
          <p className="text-xs text-slate-400">Today's estimated spend</p>
          <p className="mt-1 text-3xl font-semibold text-slate-100">
            {today ? formatUSD(today.estimated_cost) : '—'}
          </p>
          {dailyLimit > 0 && today && (
            <>
              <div className="mt-3 h-2 w-full overflow-hidden rounded-full bg-slate-800">
                <div
                  className={`h-full ${
                    limitReached ? 'bg-red-500/80' : limitPct > 80 ? 'bg-amber-400/80' : 'bg-emerald-500/80'
                  }`}
                  style={{ width: `${limitPct}%` }}
                />
              </div>
              <p className="mt-1 text-xs text-slate-500">
                {formatUSD(today.estimated_cost)} of {formatUSD(dailyLimit)} ({limitPct.toFixed(0)}%)
                {limitReached && <span className="ml-2 text-red-300">· auto-dispatch paused</span>}
              </p>
            </>
          )}
        </div>
        <div>
          <p className="text-xs text-slate-400">Tokens used today</p>
          <p className="mt-1 text-2xl font-semibold text-slate-100">
            {today ? formatTokens(today.input_tokens + today.output_tokens) : '—'}
          </p>
          {today && (
            <p className="mt-1 text-xs text-slate-500">
              in {formatTokens(today.input_tokens)} · out {formatTokens(today.output_tokens)}
            </p>
          )}
        </div>
        <div>
          <p className="text-xs text-slate-400">Daily limit</p>
          <p className="mt-1 text-2xl font-semibold text-slate-100">
            {dailyLimit > 0 ? formatUSD(dailyLimit) : 'no limit'}
          </p>
          {status.data?.copilot_request_limit ? (
            <p className="mt-1 text-xs text-slate-500">
              Copilot: {(status.data.copilot_premium_requests ?? 0).toFixed(1)} /{' '}
              {status.data.copilot_request_limit} premium req
              {status.data.copilot_limit_reached && (
                <span className="ml-1 text-red-300">· limit reached</span>
              )}
            </p>
          ) : null}
        </div>
      </section>

      <Pane
        title="Per-provider spend (today)"
        icon={<Coins size={16} className="text-emerald-400" aria-hidden />}
        count={providers.length}
        loading={costs.loading}
        error={costs.error}
      >
        {providers.length === 0 ? (
          <EmptyState message="No provider activity recorded today." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {providers.map((p) => (
              <li key={p.provider} className="px-4 py-3">
                <div className="flex flex-wrap items-baseline gap-2">
                  <span className="font-mono text-sm text-slate-100">{p.provider}</span>
                  <span className="ml-auto text-sm font-semibold text-emerald-300">
                    {formatUSD(p.estimated_cost)}
                  </span>
                </div>
                <p className="mt-0.5 text-xs text-slate-500">
                  in {formatTokens(p.input_tokens)} · out {formatTokens(p.output_tokens)} · cache r{' '}
                  {formatTokens(p.cache_read)} · cache w {formatTokens(p.cache_write)}
                </p>
              </li>
            ))}
          </ul>
        )}
      </Pane>

      <Pane
        title="Recent days"
        icon={<Coins size={16} className="text-amber-400" aria-hidden />}
        count={recent.length}
        loading={costs.loading}
        error={costs.error}
      >
        {recent.length === 0 ? (
          <EmptyState message="No daily cost records yet." />
        ) : (
          <table className="w-full text-sm">
            <thead className="text-left text-xs uppercase tracking-wide text-slate-500">
              <tr>
                <th className="px-4 py-2">Date</th>
                <th className="px-4 py-2">Input</th>
                <th className="px-4 py-2">Output</th>
                <th className="px-4 py-2 text-right">Estimated cost</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800">
              {recent.map((row) => (
                <tr key={row.date}>
                  <td className="px-4 py-2 font-mono text-slate-300">{row.date}</td>
                  <td className="px-4 py-2 text-slate-400">{formatTokens(row.input_tokens)}</td>
                  <td className="px-4 py-2 text-slate-400">{formatTokens(row.output_tokens)}</td>
                  <td className="px-4 py-2 text-right font-medium text-emerald-300">
                    {formatUSD(row.estimated_cost)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Pane>

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}
