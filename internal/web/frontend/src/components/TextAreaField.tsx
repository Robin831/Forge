import { useCallback, useEffect, useRef, useState } from 'react'

interface TextAreaFieldProps {
  // value is the source-of-truth value from the parent. The field mirrors it
  // into local editable state and only re-syncs from props while the textarea
  // is not focused, so the 30s poll never clobbers a value mid-typing.
  value: string
  // onChange receives the committed string on blur (multi-line input keeps
  // Enter for newlines, so there is no Enter-to-commit). It may return a
  // promise; while pending the textarea is disabled, and the local value
  // reverts to `value` if the promise rejects.
  onChange: (next: string) => void | Promise<void>
  placeholder?: string
  rows?: number
  disabled?: boolean
  id?: string
  'aria-label'?: string
}

// TextAreaField is the multi-line counterpart of TextField, used for `string`
// settings the schema marks as long-form (e.g. the Wicket triage prompt). It
// commits on blur — Enter inserts a newline rather than committing — and shares
// TextField's optimistic / disable-while-pending / revert-on-reject contract.
export default function TextAreaField({
  value,
  onChange,
  placeholder,
  rows = 4,
  disabled,
  id,
  'aria-label': ariaLabel,
}: TextAreaFieldProps) {
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
    <textarea
      id={id}
      aria-label={ariaLabel}
      value={text}
      placeholder={placeholder}
      rows={rows}
      disabled={isDisabled}
      onChange={(e) => setText(e.target.value)}
      onFocus={() => setFocused(true)}
      onBlur={() => {
        setFocused(false)
        commit()
      }}
      className="w-64 resize-y rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-sm text-slate-200 placeholder:text-slate-600 focus:border-emerald-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
    />
  )
}
