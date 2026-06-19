import { useState } from 'react'
import { ChevronDown, ChevronUp, Plus, X } from 'lucide-react'

interface ChainListProps {
  // items is the ordered list to render. ChainList is purely presentational: it
  // never mutates the list itself, it only emits intents (onAdd/onRemove/onMove)
  // so the owning field can run them through its async-commit contract.
  items: string[]
  // onAdd fires with an already-trimmed, non-empty, non-duplicate value when the
  // user commits the draft input (Enter or the add button).
  onAdd: (value: string) => void
  // onRemove fires with the index of the row to drop.
  onRemove: (index: number) => void
  // onMove fires with the index and the direction to reorder it. The owner is
  // responsible for ignoring out-of-range moves (ChainList already disables the
  // boundary buttons).
  onMove: (index: number, direction: 'up' | 'down') => void
  disabled?: boolean
  placeholder?: string
  // addLabel is the accessible name of the add control (e.g. "Add provider").
  addLabel?: string
  // idPrefix namespaces the draft input id so multiple ChainLists on one page
  // (e.g. one per stage in ProviderMapField) keep unique ids.
  idPrefix?: string
}

// ChainList renders an ordered, editable list of strings with add / remove /
// reorder affordances. It owns only the draft-input text; every committed change
// is delegated to the parent via callbacks, so the parent can keep a single
// optimistic/pending lock over the whole list (matching the NumberField /
// SelectField async-onChange pattern used across the Settings controls).
export default function ChainList({
  items,
  onAdd,
  onRemove,
  onMove,
  disabled,
  placeholder,
  addLabel = 'Add item',
  idPrefix = 'chain',
}: ChainListProps) {
  const [draft, setDraft] = useState('')

  const add = () => {
    if (disabled) return
    const trimmed = draft.trim()
    // Reject empty input and duplicates — the backend forbids empty elements and
    // a provider chain has no use for repeats.
    if (!trimmed || items.includes(trimmed)) return
    onAdd(trimmed)
    setDraft('')
  }

  const inputId = `${idPrefix}-add`

  return (
    <div className="flex flex-col gap-2">
      {items.length > 0 && (
        <ul className="flex flex-col gap-1.5">
          {items.map((item, i) => (
            <li key={item} className="flex items-center gap-1.5">
              <span className="flex-1 truncate rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-sm text-slate-200">
                {item}
              </span>
              <button
                type="button"
                disabled={disabled || i === 0}
                onClick={() => onMove(i, 'up')}
                aria-label={`Move ${item} up`}
                className="rounded-md border border-slate-700 p-1 text-slate-400 transition-colors hover:border-slate-600 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-40"
              >
                <ChevronUp size={14} aria-hidden />
              </button>
              <button
                type="button"
                disabled={disabled || i === items.length - 1}
                onClick={() => onMove(i, 'down')}
                aria-label={`Move ${item} down`}
                className="rounded-md border border-slate-700 p-1 text-slate-400 transition-colors hover:border-slate-600 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-40"
              >
                <ChevronDown size={14} aria-hidden />
              </button>
              <button
                type="button"
                disabled={disabled}
                onClick={() => onRemove(i)}
                aria-label={`Remove ${item}`}
                className="rounded-md border border-slate-700 p-1 text-slate-400 transition-colors hover:border-rose-500/60 hover:text-rose-300 focus:outline-none focus-visible:ring-1 focus-visible:ring-rose-400 disabled:cursor-not-allowed disabled:opacity-40"
              >
                <X size={14} aria-hidden />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex items-center gap-1.5">
        <input
          type="text"
          id={inputId}
          value={draft}
          placeholder={placeholder}
          disabled={disabled}
          aria-label={addLabel}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add()
            }
          }}
          className="w-44 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-sm text-slate-200 placeholder:text-slate-600 focus:border-emerald-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
        />
        <button
          type="button"
          disabled={disabled || draft.trim() === ''}
          onClick={add}
          aria-label={addLabel}
          className="inline-flex items-center gap-1 rounded-md border border-slate-700 px-2 py-1 text-xs font-medium text-slate-300 transition-colors hover:border-emerald-400/60 hover:text-slate-100 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <Plus size={12} aria-hidden />
          Add
        </button>
      </div>
    </div>
  )
}

// reorder returns a copy of arr with the element at index i swapped one slot in
// the given direction. Out-of-range moves return the original array unchanged.
// Exported so ListField and ProviderMapField share one reorder implementation.
export function reorder(
  arr: string[],
  i: number,
  direction: 'up' | 'down',
): string[] {
  const j = direction === 'up' ? i - 1 : i + 1
  if (j < 0 || j >= arr.length) return arr
  const copy = [...arr]
  ;[copy[i], copy[j]] = [copy[j], copy[i]]
  return copy
}
