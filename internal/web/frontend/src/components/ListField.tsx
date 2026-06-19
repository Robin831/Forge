import { useCallback, useEffect, useRef, useState } from 'react'
import { RotateCcw } from 'lucide-react'
import ChainList, { reorder } from './ChainList'

interface ListFieldProps {
  // value is the source-of-truth list from the parent. `null` means "inherit"
  // (the per-anvil override is unset and follows the global/default); when
  // `inheritable` is false it is treated the same as an empty list.
  value: string[] | null
  // onChange receives the full committed list, or `null` to clear a per-anvil
  // override (inherit). It may return a promise; while pending every control is
  // disabled. If the promise rejects, the optimistic list reverts to `value`.
  // This mirrors the NumberField / SelectField async-onChange contract.
  onChange: (next: string[] | null) => void | Promise<void>
  // inheritable enables the null=inherit semantics used by per-anvil overrides:
  // a null value renders an "inherit" placeholder with an Override affordance,
  // and an explicit list renders an Inherit reset. Global keys leave it false.
  inheritable?: boolean
  placeholder?: string
  // addLabel is the accessible name of the add control (e.g. "Add provider").
  addLabel?: string
  disabled?: boolean
  // idPrefix namespaces the inner draft input id so multiple ListFields on one
  // page keep unique ids.
  idPrefix?: string
  'aria-label'?: string
}

// ListField is an editable []string control (add / remove / reorder) for keys
// the backend marks as `string_list` — settings.providers and the Wicket user /
// repo / label lists. It commits the whole list on every mutation through the
// same optimistic, disable-while-pending, revert-on-reject contract the scalar
// Settings controls use, and supports the per-anvil null=inherit semantics.
export default function ListField({
  value,
  onChange,
  inheritable,
  placeholder,
  addLabel = 'Add item',
  disabled,
  idPrefix = 'list',
  'aria-label': ariaLabel,
}: ListFieldProps) {
  const [optimistic, setOptimistic] = useState<string[] | null>(value)
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Re-sync from props only while idle so a poll can't clobber an in-flight edit.
  useEffect(() => {
    if (!pendingRef.current) setOptimistic(value)
  }, [value])

  const isDisabled = disabled || pending

  const commit = useCallback(
    (next: string[] | null) => {
      if (disabled || pending) return
      const previous = optimistic
      setOptimistic(next)
      pendingRef.current = true
      setPending(true)
      Promise.resolve()
        .then(() => onChange(next))
        .catch(() => {
          if (mounted.current) setOptimistic(previous)
        })
        .finally(() => {
          if (mounted.current) {
            pendingRef.current = false
            setPending(false)
          }
        })
    },
    [disabled, pending, optimistic, onChange],
  )

  const items = optimistic ?? []
  const inheriting = inheritable === true && optimistic === null

  return (
    <div role="group" aria-label={ariaLabel} className="flex flex-col gap-2">
      {inheriting ? (
        <div className="flex items-center gap-2">
          <span className="text-xs italic text-slate-500">
            Inherits global / default
          </span>
          <button
            type="button"
            disabled={isDisabled}
            onClick={() => commit([])}
            className="rounded-md border border-slate-700 px-2 py-1 text-xs font-medium text-slate-300 transition-colors hover:border-emerald-400/60 hover:text-slate-100 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
          >
            Override
          </button>
        </div>
      ) : (
        <>
          <ChainList
            items={items}
            disabled={isDisabled}
            placeholder={placeholder}
            addLabel={addLabel}
            idPrefix={idPrefix}
            onAdd={(v) => commit([...items, v])}
            onRemove={(i) => commit(items.filter((_, j) => j !== i))}
            onMove={(i, dir) => commit(reorder(items, i, dir))}
          />
          {inheritable && (
            <button
              type="button"
              disabled={isDisabled}
              onClick={() => commit(null)}
              aria-label={`Reset ${ariaLabel ?? 'list'} to inherit`}
              title="Reset to inherit the global / default value"
              className="inline-flex w-fit items-center gap-1 rounded-md border border-slate-700 px-2 py-1 text-xs font-medium text-slate-400 transition-colors hover:border-slate-600 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <RotateCcw size={12} aria-hidden />
              Inherit
            </button>
          )}
        </>
      )}
    </div>
  )
}
