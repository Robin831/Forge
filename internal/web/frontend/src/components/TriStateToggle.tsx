import { useCallback, useEffect, useRef, useState } from 'react'

// TriState models a tri-state per-anvil override: `null` means "inherit"
// (follow the global setting / built-in default), while `true`/`false` is an
// explicit on/off override. It mirrors the nullable *bool keys in the backend's
// per-anvil config contract (see internal/config AnvilSettings).
export type TriState = boolean | null

interface TriStateToggleProps {
  // value is the source-of-truth state from the parent. The control mirrors it
  // into local state so it can update optimistically while an async onChange
  // settles, then reconciles back to the parent value afterwards.
  value: TriState
  // onChange receives the desired next state. It may return a promise; while
  // that promise is pending the control is disabled and shows the optimistic
  // value. If the promise rejects, the optimistic value is reverted.
  onChange: (next: TriState) => void | Promise<void>
  // disabled blocks interaction entirely (in addition to the in-flight lock).
  disabled?: boolean
  // id wires the radiogroup to an external <label htmlFor>.
  id?: string
  // aria-label describes the group when there is no visible text label.
  'aria-label'?: string
}

// OPTIONS is the fixed Inherit / On / Off ordering rendered by the control. The
// `null` option (Inherit) comes first so the "unset, follow global" state reads
// as the neutral default.
const OPTIONS: { label: string; value: TriState }[] = [
  { label: 'Inherit', value: null },
  { label: 'On', value: true },
  { label: 'Off', value: false },
]

// optionKey renders a stable React key/test id for each TriState option.
function optionKey(value: TriState): string {
  return value === null ? 'inherit' : value ? 'on' : 'off'
}

// TriStateToggle is a dependency-free segmented control built on a native
// radiogroup of <button role="radio"> elements. It models the same async-update
// behaviour as Switch: the visual selection moves optimistically the moment an
// option is clicked, the control disables itself while the onChange promise is
// in flight, and it reverts to the previous selection if that promise rejects.
//
// Keyboard: each option is a focusable button; Space/Enter activate it. The
// focus ring and disabled styling are driven entirely by Tailwind utilities.
export default function TriStateToggle({
  value,
  onChange,
  disabled,
  id,
  'aria-label': ariaLabel,
}: TriStateToggleProps) {
  const [optimistic, setOptimistic] = useState<TriState>(value)
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Sync optimistic state only when the controlled `value` prop changes. A ref
  // guards against overwriting the optimistic value mid-flight without adding
  // `pending` as a dependency (which would re-trigger on settle and flash the
  // stale parent value before polling catches up).
  useEffect(() => {
    if (!pendingRef.current) setOptimistic(value)
  }, [value])

  const isDisabled = disabled || pending

  const select = useCallback(
    (next: TriState) => {
      if (disabled || pending || next === optimistic) return
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

  return (
    <div
      role="radiogroup"
      id={id}
      aria-label={ariaLabel}
      className="inline-flex shrink-0 rounded-md border border-slate-700 bg-slate-800/60 p-0.5"
    >
      {OPTIONS.map((opt) => {
        const active = optimistic === opt.value
        return (
          <button
            key={optionKey(opt.value)}
            type="button"
            role="radio"
            aria-checked={active}
            disabled={isDisabled}
            onClick={() => select(opt.value)}
            className={`rounded px-2.5 py-1 text-xs font-medium transition-colors duration-150 focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-400 focus-visible:ring-offset-1 focus-visible:ring-offset-slate-900 disabled:cursor-not-allowed disabled:opacity-50 ${
              active
                ? 'bg-emerald-500 text-white shadow-sm'
                : 'text-slate-300 hover:text-slate-100'
            }`}
          >
            {opt.label}
          </button>
        )
      })}
    </div>
  )
}
