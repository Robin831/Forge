import { useState } from 'react'
import { List, MoreHorizontal, Play, RotateCcw, Square } from 'lucide-react'
import { Link } from 'react-router-dom'
import { actions, type QueueItem } from '../api'
import { priorityClasses, priorityLabel } from '../lib/format'
import { useAction } from '../hooks/useAction'
import ConfirmModal from './ConfirmModal'
import Pane, { EmptyState } from './Pane'

interface QueuePaneProps {
  items: QueueItem[]
  loading: boolean
  error: string | null
  // Callback fired after a destructive action succeeds so the parent page can
  // refresh its polled data immediately rather than waiting for the next tick.
  onActionSuccess?: () => void
}

type DialogKind = 'dispatch' | 'stop' | 'clarify'

interface DialogState {
  kind: DialogKind
  bead: QueueItem
}

export default function QueuePane({
  items,
  loading,
  error,
  onActionSuccess,
}: QueuePaneProps) {
  const { run, busy } = useAction()
  const [dialog, setDialog] = useState<DialogState | null>(null)
  const [openMenu, setOpenMenu] = useState<string | null>(null)

  const handleRetry = (item: QueueItem) => {
    setOpenMenu(null)
    void run(() => actions.retry(item.bead_id, item.anvil), {
      successMessage: `Retry triggered for ${item.bead_id}`,
      onSuccess: onActionSuccess,
    })
  }

  const handleUnclarify = (item: QueueItem) => {
    setOpenMenu(null)
    void run(() => actions.unclarify(item.bead_id, item.anvil), {
      successMessage: `Cleared clarification for ${item.bead_id}`,
      onSuccess: onActionSuccess,
    })
  }

  const closeDialog = () => setDialog(null)

  const handleConfirm = async (input: string) => {
    if (!dialog) return
    const { bead, kind } = dialog
    if (kind === 'dispatch') {
      const ok = await run(() => actions.dispatch(bead.bead_id, bead.anvil, false), {
        successMessage: `Dispatched ${bead.bead_id}`,
        onSuccess: onActionSuccess,
      })
      if (ok) closeDialog()
    } else if (kind === 'stop') {
      const ok = await run(() => actions.stop(bead.bead_id, bead.anvil, input), {
        successMessage: `Stopped ${bead.bead_id}`,
        onSuccess: onActionSuccess,
      })
      if (ok) closeDialog()
    } else if (kind === 'clarify') {
      if (!input.trim()) return
      const ok = await run(() => actions.clarify(bead.bead_id, bead.anvil, input.trim()), {
        successMessage: `Marked ${bead.bead_id} as needing clarification`,
        onSuccess: onActionSuccess,
      })
      if (ok) closeDialog()
    }
  }

  return (
    <>
      <Pane
        title="Queue"
        icon={<List size={16} className="text-cyan-400" aria-hidden />}
        count={items.length}
        loading={loading}
        error={error}
      >
        {items.length === 0 && !loading ? (
          <EmptyState message="No beads in queue." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {items.map((item) => (
              <li key={`${item.anvil}:${item.bead_id}`} className="px-4 py-3">
                <div className="flex items-start gap-2">
                  <span
                    className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${priorityClasses(item.priority)}`}
                  >
                    {priorityLabel(item.priority)}
                  </span>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium text-slate-100">
                      {item.title || item.bead_id}
                    </p>
                    <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                      <Link
                        to={`/bead/${item.bead_id}?anvil=${encodeURIComponent(item.anvil)}`}
                        className="font-mono text-slate-400 hover:text-amber-300"
                      >
                        {item.bead_id}
                      </Link>
                      <span aria-hidden>·</span>
                      <span>{item.anvil}</span>
                      {item.section && (
                        <>
                          <span aria-hidden>·</span>
                          <span className="capitalize">{item.section}</span>
                        </>
                      )}
                    </p>
                    {item.labels.length > 0 && (
                      <div className="mt-1.5 flex flex-wrap gap-1">
                        {item.labels.slice(0, 4).map((label) => (
                          <span
                            key={label}
                            className="rounded-full bg-slate-800 px-2 py-0.5 text-[10px] text-slate-300"
                          >
                            {label}
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                  <QueueActions
                    item={item}
                    busy={busy}
                    open={openMenu === item.bead_id}
                    onToggle={() =>
                      setOpenMenu(openMenu === item.bead_id ? null : item.bead_id)
                    }
                    onDispatch={() => {
                      setOpenMenu(null)
                      setDialog({ kind: 'dispatch', bead: item })
                    }}
                    onRetry={() => handleRetry(item)}
                    onClarify={() => {
                      setOpenMenu(null)
                      setDialog({ kind: 'clarify', bead: item })
                    }}
                    onUnclarify={() => handleUnclarify(item)}
                    onStop={() => {
                      setOpenMenu(null)
                      setDialog({ kind: 'stop', bead: item })
                    }}
                  />
                </div>
              </li>
            ))}
          </ul>
        )}
      </Pane>

      <ConfirmModal
        open={dialog?.kind === 'dispatch'}
        title="Dispatch bead?"
        message={
          dialog?.kind === 'dispatch'
            ? `This will start a Smith worker for ${dialog.bead.bead_id} (${dialog.bead.anvil}).`
            : ''
        }
        confirmLabel="Dispatch"
        tone="primary"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />
      <ConfirmModal
        open={dialog?.kind === 'stop'}
        title="Stop bead?"
        message={
          dialog?.kind === 'stop'
            ? `This kills any active worker on ${dialog.bead.bead_id} and prevents re-dispatch until cleared.`
            : ''
        }
        confirmLabel="Stop"
        tone="danger"
        inputLabel="Reason (optional)"
        inputPlaceholder="manually stopped"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />
      <ConfirmModal
        open={dialog?.kind === 'clarify'}
        title="Mark needs clarification?"
        message={
          dialog?.kind === 'clarify'
            ? `${dialog.bead.bead_id} will be skipped by the poller until cleared.`
            : ''
        }
        confirmLabel="Mark"
        tone="primary"
        inputLabel="Reason"
        inputPlaceholder="What needs clarification?"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />
    </>
  )
}

interface QueueActionsProps {
  item: QueueItem
  busy: boolean
  open: boolean
  onToggle: () => void
  onDispatch: () => void
  onRetry: () => void
  onClarify: () => void
  onUnclarify: () => void
  onStop: () => void
}

function QueueActions({
  busy,
  open,
  onToggle,
  onDispatch,
  onRetry,
  onClarify,
  onUnclarify,
  onStop,
}: QueueActionsProps) {
  return (
    <div className="relative shrink-0">
      <button
        type="button"
        onClick={onToggle}
        disabled={busy}
        className="rounded-md border border-slate-700 bg-slate-800 p-1 text-slate-300 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-50"
        aria-label="Bead actions"
      >
        <MoreHorizontal size={14} />
      </button>
      {open && (
        <div
          role="menu"
          aria-label="Bead actions"
          className="absolute right-0 top-full z-30 mt-1 w-44 overflow-hidden rounded-md border border-slate-700 bg-slate-900 text-xs shadow-lg"
        >
          <MenuItem icon={<Play size={12} />} label="Dispatch" onClick={onDispatch} />
          <MenuItem icon={<RotateCcw size={12} />} label="Retry" onClick={onRetry} />
          <MenuItem label="Mark needs clarification" onClick={onClarify} />
          <MenuItem label="Clear clarification" onClick={onUnclarify} />
          <MenuItem
            icon={<Square size={12} />}
            label="Stop"
            tone="danger"
            onClick={onStop}
          />
        </div>
      )}
    </div>
  )
}

interface MenuItemProps {
  icon?: React.ReactNode
  label: string
  tone?: 'danger'
  onClick: () => void
}

function MenuItem({ icon, label, tone, onClick }: MenuItemProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`flex w-full items-center gap-2 px-3 py-2 text-left ${
        tone === 'danger'
          ? 'text-red-300 hover:bg-red-500/10'
          : 'text-slate-200 hover:bg-slate-800'
      }`}
      role="menuitem"
    >
      {icon}
      <span>{label}</span>
    </button>
  )
}
