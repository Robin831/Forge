import { useCallback, useEffect, useRef, useState } from 'react'

interface SwitchProps {
  // checked is the source-of-truth value coming from the parent. The component
  // mirrors it into local state so it can update optimistically while an async
  // onChange settles, then reconciles back to the parent value afterwards.
  checked: boolean
  // onChange receives the desired next value. It may return a promise; while
  // that promise is pending the switch is disabled and shows the optimistic
  // value. If the promise rejects, the optimistic value is reverted.
  onChange: (next: boolean) => void | Promise<void>
  // disabled blocks interaction entirely (in addition to the in-flight lock).
  disabled?: boolean
  // id wires the control to an external <label htmlFor>.
  id?: string
  // aria-label describes the control when there is no visible text label.
  'aria-label'?: string
}

// Switch is a standalone, dependency-free sliding on/off toggle built on a
// native <button role="switch">. It models the same async-update behaviour as
// DispatchToggle: the visual state flips optimistically the moment it is
// clicked, the control disables itself while the onChange promise is in flight,
// and it reverts to the previous value if that promise rejects.
//
// Keyboard: the button is natively focusable and Space/Enter activate it, so
// no extra key handling is required. The focus ring and disabled styling are
// driven entirely by Tailwind utilities.
export default function Switch({
  checked,
  onChange,
  disabled,
  id,
  'aria-label': ariaLabel,
}: SwitchProps) {
  const [optimistic, setOptimistic] = useState(checked)
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Sync optimistic state only when the controlled `checked` prop changes.
  // A ref guards against overwriting the optimistic value mid-flight without
  // adding `pending` as a dependency (which would re-trigger on settle and
  // flash the stale parent value before polling catches up).
  useEffect(() => {
    if (!pendingRef.current) setOptimistic(checked)
  }, [checked])

  const isDisabled = disabled || pending

  const toggle = useCallback(() => {
    if (disabled || pending) return
    const next = !optimistic
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
  }, [disabled, pending, optimistic, onChange])

  return (
    <button
      type="button"
      role="switch"
      id={id}
      aria-label={ariaLabel}
      aria-checked={optimistic}
      disabled={isDisabled}
      onClick={toggle}
      className={`relative inline-flex h-6 w-11 shrink-0 cursor-pointer items-center rounded-full border border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-400 focus-visible:ring-offset-2 focus-visible:ring-offset-slate-900 disabled:cursor-not-allowed disabled:opacity-50 ${
        optimistic ? 'bg-emerald-500' : 'bg-slate-600'
      }`}
    >
      <span
        aria-hidden
        className={`inline-block h-5 w-5 transform rounded-full bg-white shadow-sm transition-transform duration-200 ease-in-out ${
          optimistic ? 'translate-x-5' : 'translate-x-0.5'
        }`}
      />
    </button>
  )
}
