import { useCallback, useEffect, useRef, useState } from 'react'

interface SelectFieldProps {
  // value is the source-of-truth value from the parent.
  value: string
  // options is the allowed set. If `value` is not among them it is prepended as
  // a leading option so the current value still displays.
  options: string[]
  // onChange receives the chosen option (committed immediately on change). It
  // may return a promise; while pending the select is disabled. If the promise
  // rejects, the optimistic value reverts to `value`.
  onChange: (next: string) => void | Promise<void>
  disabled?: boolean
  id?: string
  'aria-label'?: string
}

// SelectField is a styled native <select> that commits the chosen option
// immediately, mirroring Switch's async-onChange contract: optimistic update,
// disable while the onChange promise is in flight, and revert if it rejects.
export default function SelectField({
  value,
  options,
  onChange,
  disabled,
  id,
  'aria-label': ariaLabel,
}: SelectFieldProps) {
  const [optimistic, setOptimistic] = useState(value)
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  useEffect(() => {
    if (!pendingRef.current) setOptimistic(value)
  }, [value])

  const isDisabled = disabled || pending

  // Include the current value as a leading option when it is not in the
  // allowed set, so it still renders rather than silently snapping to the
  // first option.
  const renderedOptions = options.includes(optimistic)
    ? options
    : [optimistic, ...options]

  const select = useCallback(
    (next: string) => {
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
    <select
      id={id}
      aria-label={ariaLabel}
      value={optimistic}
      disabled={isDisabled}
      onChange={(e) => select(e.target.value)}
      className="rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-sm text-slate-200 focus:border-emerald-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
    >
      {renderedOptions.map((opt) => (
        <option key={opt} value={opt}>
          {opt}
        </option>
      ))}
    </select>
  )
}
