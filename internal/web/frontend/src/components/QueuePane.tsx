import { useMemo, useRef, useState } from 'react'
import { ChevronDown, ChevronRight, List, MoreHorizontal, Play, RotateCcw, Square, X } from 'lucide-react'
import { Link } from 'react-router-dom'
import { actions, type QueueItem } from '../api'
import { priorityClasses, priorityLabel, relativeTime } from '../lib/format'
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

type BucketKey = 'ready' | 'unlabeled' | 'in_progress'

const BUCKET_ORDER: BucketKey[] = ['ready', 'unlabeled', 'in_progress']

const BUCKET_LABEL: Record<BucketKey, string> = {
  ready: 'Ready',
  unlabeled: 'Unlabeled',
  in_progress: 'In progress',
}

export type SortKey =
  | 'priority-asc'
  | 'updated-desc'
  | 'updated-asc'
  | 'created-desc'
  | 'created-asc'
  | 'title-asc'

const SORT_OPTIONS: ReadonlyArray<{ value: SortKey; label: string }> = [
  { value: 'priority-asc', label: 'Priority (asc)' },
  { value: 'updated-desc', label: 'Last updated (newest first)' },
  { value: 'updated-asc', label: 'Last updated (oldest first)' },
  { value: 'created-desc', label: 'Created (newest first)' },
  { value: 'created-asc', label: 'Created (oldest first)' },
  { value: 'title-asc', label: 'Title (A→Z)' },
]

// parseTimestamp converts an ISO string to a numeric epoch for comparison.
// Missing/unparseable values become NaN and are treated as "oldest" by
// compareTimestamps so the timestamp-based options degrade gracefully when the
// backend hasn't filled in created_at / updated_at yet.
function parseTimestamp(value: string | undefined): number {
  if (!value) return NaN
  const t = Date.parse(value)
  return Number.isNaN(t) ? NaN : t
}

// compareTimestamps sorts by parsed epoch. NaN values are treated as "oldest":
// they sort first for ascending order (oldest first) and last for descending
// order (newest first), so UI labels match the actual item ordering.
function compareTimestamps(a: number, b: number, direction: 'asc' | 'desc'): number {
  const aMissing = Number.isNaN(a)
  const bMissing = Number.isNaN(b)
  if (aMissing && bMissing) return 0
  if (aMissing) return direction === 'asc' ? -1 : 1
  if (bMissing) return direction === 'asc' ? 1 : -1
  return direction === 'asc' ? a - b : b - a
}

export function sortItems(items: QueueItem[], sortKey: SortKey): QueueItem[] {
  const copy = items.slice()
  switch (sortKey) {
    case 'priority-asc':
      copy.sort((a, b) => a.priority - b.priority)
      return copy
    case 'updated-desc':
    case 'updated-asc': {
      const dir = sortKey === 'updated-desc' ? 'desc' : 'asc'
      const ts = new Map(copy.map((item) => [item, parseTimestamp(item.updated_at)]))
      copy.sort((a, b) => compareTimestamps(ts.get(a)!, ts.get(b)!, dir))
      return copy
    }
    case 'created-desc':
    case 'created-asc': {
      const dir = sortKey === 'created-desc' ? 'desc' : 'asc'
      const ts = new Map(copy.map((item) => [item, parseTimestamp(item.created_at)]))
      copy.sort((a, b) => compareTimestamps(ts.get(a)!, ts.get(b)!, dir))
      return copy
    }
    case 'title-asc':
      copy.sort((a, b) =>
        (a.title || a.bead_id).localeCompare(b.title || b.bead_id, undefined, {
          sensitivity: 'base',
        }),
      )
      return copy
  }
}

interface AnvilGroup {
  anvil: string
  total: number
  buckets: Record<BucketKey, QueueItem[]>
}

// bucketFor maps a QueueItem's server-classified `section` to one of the three
// UI buckets. Anything we don't recognise (older payloads, unexpected values)
// falls into Ready so it remains visible — losing items silently would be worse
// than putting them in the wrong bucket.
function bucketFor(item: QueueItem): BucketKey {
  switch (item.section) {
    case 'in_progress':
      return 'in_progress'
    case 'unlabeled':
      return 'unlabeled'
    default:
      return 'ready'
  }
}

// Encode an arbitrary anvil name into a valid HTML id token.
// encodeURIComponent is deterministic and collision-free; replacing % with _
// keeps the result free of characters that are invalid in id tokens.
function anvilDomId(name: string): string {
  return `anvil-body-${encodeURIComponent(name).replace(/%/g, '_')}`
}

export function groupQueueItems(items: QueueItem[]): AnvilGroup[] {
  const byAnvil = new Map<string, AnvilGroup>()
  for (const item of items) {
    let group = byAnvil.get(item.anvil)
    if (!group) {
      group = {
        anvil: item.anvil,
        total: 0,
        buckets: { ready: [], unlabeled: [], in_progress: [] },
      }
      byAnvil.set(item.anvil, group)
    }
    group.buckets[bucketFor(item)].push(item)
    group.total += 1
  }
  // Preserve the upstream order within each bucket (daemon already sorts by
  // priority asc, then created_at desc). Anvils are alphabetised so the layout
  // is stable across polls.
  return Array.from(byAnvil.values()).sort((a, b) => a.anvil.localeCompare(b.anvil))
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
  const [filter, setFilter] = useState('')
  const filterInputRef = useRef<HTMLInputElement>(null)
  const [sortKey, setSortKey] = useState<SortKey>('priority-asc')
  // Expand state is keyed by anvil name (`anvil:<name>`) and bucket
  // (`bucket:<anvil>:<bucket>`). Missing keys mean collapsed — both anvils and
  // buckets start closed so the operator sees a compact summary first. State
  // lives in component-local useState (per the bead spec: no localStorage).
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const filteredItems = useMemo(() => {
    const q = filter.trim().toLowerCase()
    if (!q) return items
    return items.filter((item) => {
      if (item.bead_id.toLowerCase().includes(q)) return true
      if (item.title && item.title.toLowerCase().includes(q)) return true
      if (item.labels.some((label) => label.toLowerCase().includes(q))) return true
      return false
    })
  }, [items, filter])

  const groups = useMemo(() => {
    const grouped = groupQueueItems(filteredItems)
    return grouped.map((group) => ({
      ...group,
      buckets: Object.fromEntries(
        BUCKET_ORDER.map((bucket) => [bucket, sortItems(group.buckets[bucket], sortKey)]),
      ) as Record<BucketKey, QueueItem[]>,
    }))
  }, [filteredItems, sortKey])

  const toggle = (key: string) =>
    setExpanded((prev) => ({ ...prev, [key]: !prev[key] }))

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

  const totalCount = filter.trim() ? filteredItems.length : items.length

  return (
    <>
      <Pane
        title="Queue"
        icon={<List size={16} className="text-cyan-400" aria-hidden />}
        count={totalCount}
        loading={loading}
        error={error}
        headerExtra={
          <div className="flex flex-col gap-2 sm:flex-row">
            <div className="relative w-full">
              <input
                ref={filterInputRef}
                type="text"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="Filter beads (id, title, label)"
                aria-label="Filter beads"
                className={`w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1 text-sm text-slate-100 placeholder:text-slate-500 focus:border-amber-400/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300 ${filter.trim() ? 'pr-7' : ''}`}
              />
              {filter.trim() && (
                <button
                  type="button"
                  onClick={() => { setFilter(''); filterInputRef.current?.focus() }}
                  aria-label="Clear filter"
                  className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-slate-400 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
                >
                  <X size={14} aria-hidden />
                </button>
              )}
            </div>
            <select
              value={sortKey}
              onChange={(e) => setSortKey(e.target.value as SortKey)}
              aria-label="Sort queue"
              data-testid="queue-sort-select"
              className="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1 text-sm text-slate-100 focus:border-amber-400/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300 sm:w-auto"
            >
              {SORT_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
          </div>
        }
      >
        {items.length === 0 && !loading ? (
          <EmptyState message="No beads in queue." />
        ) : groups.length === 0 && filter.trim() ? (
          <EmptyState message="No beads match the filter." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {groups.map((group) => {
              const anvilKey = `anvil:${group.anvil}`
              const anvilOpen = !!expanded[anvilKey]
              return (
                <li key={group.anvil}>
                  <button
                    type="button"
                    onClick={() => toggle(anvilKey)}
                    aria-expanded={anvilOpen}
                    aria-controls={anvilDomId(group.anvil)}
                    className="flex w-full items-center gap-2 px-4 py-2.5 text-left text-sm font-semibold text-slate-100 hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
                  >
                    {anvilOpen ? (
                      <ChevronDown size={14} className="text-slate-400" aria-hidden />
                    ) : (
                      <ChevronRight size={14} className="text-slate-400" aria-hidden />
                    )}
                    <span className="truncate">{group.anvil}</span>
                    <span className="ml-auto rounded-full bg-slate-800 px-2 py-0.5 text-xs font-normal text-slate-300">
                      {group.total}
                    </span>
                  </button>
                  {anvilOpen && (
                    <div id={anvilDomId(group.anvil)} className="border-t border-slate-800/60 bg-slate-950/40">
                      {BUCKET_ORDER.flatMap((bucket) => {
                        const bucketItems = group.buckets[bucket]
                        if (bucketItems.length === 0) return []
                        const bucketKey = `bucket:${group.anvil}:${bucket}`
                        const bucketOpen = !!expanded[bucketKey]
                        return [
                          <BucketSection
                            key={bucketKey}
                            label={BUCKET_LABEL[bucket]}
                            count={bucketItems.length}
                            open={bucketOpen}
                            onToggle={() => toggle(bucketKey)}
                            items={bucketItems}
                            busy={busy}
                            openMenu={openMenu}
                            setOpenMenu={setOpenMenu}
                            setDialog={setDialog}
                            onRetry={handleRetry}
                            onUnclarify={handleUnclarify}
                          />,
                        ]
                      })}
                    </div>
                  )}
                </li>
              )
            })}
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

interface BucketSectionProps {
  label: string
  count: number
  open: boolean
  onToggle: () => void
  items: QueueItem[]
  busy: boolean
  openMenu: string | null
  setOpenMenu: (key: string | null) => void
  setDialog: (state: DialogState) => void
  onRetry: (item: QueueItem) => void
  onUnclarify: (item: QueueItem) => void
}

function BucketSection({
  label,
  count,
  open,
  onToggle,
  items,
  busy,
  openMenu,
  setOpenMenu,
  setDialog,
  onRetry,
  onUnclarify,
}: BucketSectionProps) {
  return (
    <div>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        className="flex w-full items-center gap-2 px-4 py-2 pl-8 text-left text-xs font-semibold uppercase tracking-wide text-slate-300 hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
      >
        {open ? (
          <ChevronDown size={12} className="text-slate-500" aria-hidden />
        ) : (
          <ChevronRight size={12} className="text-slate-500" aria-hidden />
        )}
        <span>{`${label} (${count})`}</span>
      </button>
      {open && (
        <ul className="divide-y divide-slate-800/60 border-t border-slate-800/60">
          {items.map((item) => {
            const menuKey = `${item.anvil}:${item.bead_id}`
            return (
              <li key={menuKey} className="px-4 py-3 pl-8">
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
                      {item.assignee && (
                        <>
                          <span aria-hidden>·</span>
                          <span>@{item.assignee}</span>
                        </>
                      )}
                      {item.updated_at && (
                        <>
                          <span aria-hidden>·</span>
                          <span title={item.updated_at}>
                            Updated {relativeTime(item.updated_at)}
                          </span>
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
                    open={openMenu === menuKey}
                    onToggle={() =>
                      setOpenMenu(openMenu === menuKey ? null : menuKey)
                    }
                    onDispatch={() => {
                      setOpenMenu(null)
                      setDialog({ kind: 'dispatch', bead: item })
                    }}
                    onRetry={() => onRetry(item)}
                    onClarify={() => {
                      setOpenMenu(null)
                      setDialog({ kind: 'clarify', bead: item })
                    }}
                    onUnclarify={() => onUnclarify(item)}
                    onStop={() => {
                      setOpenMenu(null)
                      setDialog({ kind: 'stop', bead: item })
                    }}
                  />
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </div>
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
