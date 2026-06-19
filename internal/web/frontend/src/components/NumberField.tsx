import { useCallback, useEffect, useRef, useState } from 'react'

interface NumberFieldProps {
  // value is the source-of-truth value from the parent. The field mirrors it
  // into a local editable string so the user can type freely without the 30s
  // poll clobbering mid-edit; it only re-syncs from props while the input is
  // not focused (and not dirty).
  value: number
  // onChange receives the committed numeric value (on blur or Enter, never on
  // every keystroke). It may return a promise; while pending the input is
  // disabled. If the promise rejects, the local value reverts to `value`.
  onChange: (next: number) => void | Promise<void>
  min?: number
  max?: number
  // step controls the native stepper granularity. Defaults to 1 (integer
  // semantics); pass 'any' for float keys.
  step?: number | 'any'
  // unit is an optional display suffix rendered after the input (e.g. "USD").
  unit?: string
  disabled?: boolean
  id?: string
  'aria-label'?: string
}

// NumberField is a numeric <input type="number"> that commits on blur or Enter
// rather than on every keystroke, so the parent's poll-driven re-render never
// clobbers a value mid-typing. It mirrors Switch's async-onChange contract:
// optimistic local edit, disable while the onChange promise is in flight, and
// revert to the controlled value if that promise rejects.
export default function NumberField({
  value,
  onChange,
  min,
  max,
  step = 1,
  unit,
  disabled,
  id,
  'aria-label': ariaLabel,
}: NumberFieldProps) {
  const [text, setText] = useState(String(value))
  const [pending, setPending] = useState(false)
  const [focused, setFocused] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Re-sync from props only when the input is idle (not focused, nothing in
  // flight) so a poll can't overwrite what the user is typing.
  useEffect(() => {
    if (!focused && !pendingRef.current) setText(String(value))
  }, [value, focused])

  const isDisabled = disabled || pending

  const commit = useCallback(() => {
    if (disabled || pending) return
    const parsed = Number(text)
    // Reject empty / non-numeric input by reverting to the controlled value.
    if (text.trim() === '' || Number.isNaN(parsed)) {
      setText(String(value))
      return
    }
    // No change: snap back to the canonical formatting and skip the request.
    if (parsed === value) {
      setText(String(value))
      return
    }
    pendingRef.current = true
    setPending(true)
    Promise.resolve()
      .then(() => onChange(parsed))
      .catch(() => {
        if (mounted.current) setText(String(value))
      })
      .finally(() => {
        if (mounted.current) {
          pendingRef.current = false
          setPending(false)
        }
      })
  }, [disabled, pending, text, value, onChange])

  return (
    <div className="inline-flex items-center gap-1.5">
      <input
        type="number"
        id={id}
        aria-label={ariaLabel}
        value={text}
        min={min}
        max={max}
        step={step}
        disabled={isDisabled}
        onChange={(e) => setText(e.target.value)}
        onFocus={() => setFocused(true)}
        onBlur={() => {
          setFocused(false)
          commit()
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            e.currentTarget.blur()
          }
        }}
        className="w-20 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-right text-sm text-slate-200 focus:border-emerald-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
      />
      {unit && <span className="text-xs text-slate-500">{unit}</span>}
    </div>
  )
}
