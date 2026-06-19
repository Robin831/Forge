import { useCallback, useEffect, useRef, useState } from 'react'

interface TextFieldProps {
  // value is the source-of-truth value from the parent. The field mirrors it
  // into local editable state and only re-syncs from props while the input is
  // not focused, so the 30s poll never clobbers a value mid-typing.
  value: string
  // onChange receives the committed string (on blur or Enter, never on every
  // keystroke). It may return a promise; while pending the input is disabled.
  // If the promise rejects, the local value reverts to `value`.
  onChange: (next: string) => void | Promise<void>
  placeholder?: string
  disabled?: boolean
  id?: string
  'aria-label'?: string
}

// TextField is a text <input> that commits on blur or Enter, mirroring
// Switch's async-onChange contract: optimistic local edit, disable while the
// onChange promise is in flight, and revert to the controlled value if that
// promise rejects.
export default function TextField({
  value,
  onChange,
  placeholder,
  disabled,
  id,
  'aria-label': ariaLabel,
}: TextFieldProps) {
  const [text, setText] = useState(value)
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

  // Re-sync from props only when idle so a poll can't overwrite typing.
  useEffect(() => {
    if (!focused && !pendingRef.current) setText(value)
  }, [value, focused])

  const isDisabled = disabled || pending

  const commit = useCallback(() => {
    if (disabled || pending) return
    if (text === value) return
    pendingRef.current = true
    setPending(true)
    Promise.resolve()
      .then(() => onChange(text))
      .catch(() => {
        if (mounted.current) setText(value)
      })
      .finally(() => {
        if (mounted.current) {
          pendingRef.current = false
          setPending(false)
        }
      })
  }, [disabled, pending, text, value, onChange])

  return (
    <input
      type="text"
      id={id}
      aria-label={ariaLabel}
      value={text}
      placeholder={placeholder}
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
      className="w-44 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-sm text-slate-200 placeholder:text-slate-600 focus:border-emerald-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
    />
  )
}
