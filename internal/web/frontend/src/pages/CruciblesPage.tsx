import { useApiPoll } from '../hooks/useApiPoll'
import type { CruciblesResponse, StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import CruciblesPane from '../components/CruciblesPane'

const POLL_INTERVAL_MS = 5000

export default function CruciblesPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const crucibles = useApiPoll<CruciblesResponse>('/api/crucibles', POLL_INTERVAL_MS)

  return (
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <main>
        <CruciblesPane
          crucibles={crucibles.data?.crucibles ?? []}
          loading={crucibles.loading}
          error={crucibles.error}
        />
      </main>

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}
