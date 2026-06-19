import { useCallback, useEffect, useId, useRef, useState } from 'react'

// GO_DURATION_UNITS are the time-unit suffixes Go's time.ParseDuration accepts.
// Ordered longest-first within each ambiguous prefix group ("ms" before "s",
// "us"/"ns" before "s") so a greedy left-match consumes the intended unit. Both
// micro spellings Go allows are included: "us", the ASCII micro sign "µs"
// (U+00B5) and the Greek small mu "μs" (U+03BC).
const GO_DURATION_UNITS = ['ns', 'us', 'µs', 'μs', 'ms', 's', 'm', 'h']

// isValidGoDuration reports whether s parses as a Go duration string, mirroring
// the backend's time.ParseDuration check (internal/web/forge_config.go). A
// duration is an optionally-signed sequence of decimal numbers — each with an
// optional fraction — every one carrying a unit suffix, e.g. "300ms", "-1.5h",
// "2h45m". A bare "0" (optionally signed) is the one number permitted without a
// unit. Overflow is not validated client-side; the backend remains authoritative.
export function isValidGoDuration(s: string): boolean {
  let rest = s
  if (rest === '') return false

  // Optional leading sign.
  if (rest[0] === '+' || rest[0] === '-') rest = rest.slice(1)

  // Special case: a bare zero needs no unit (Go accepts "0", "+0", "-0").
  if (rest === '0') return true
  if (rest === '') return false

  while (rest !== '') {
    // Each segment must start with a digit or a decimal point.
    if (rest[0] !== '.' && (rest[0] < '0' || rest[0] > '9')) return false

    let i = 0
    let sawDigit = false
    while (i < rest.length && rest[i] >= '0' && rest[i] <= '9') {
      i++
      sawDigit = true
    }
    if (i < rest.length && rest[i] === '.') {
      i++
      while (i < rest.length && rest[i] >= '0' && rest[i] <= '9') {
        i++
        sawDigit = true
      }
    }
    // A lone "." with no digits on either side is not a number.
    if (!sawDigit) return false

    const afterNumber = rest.slice(i)
    const unit = GO_DURATION_UNITS.find((u) => afterNumber.startsWith(u))
    if (!unit) return false
    rest = afterNumber.slice(unit.length)
  }
  return true
}

interface DurationFieldProps {
  // value is the source-of-truth Go duration string from the parent (e.g.
  // "5m0s"). The field mirrors it into local editable state and only re-syncs
  // from props while idle, so the 30s poll never clobbers a value mid-typing.
  value: string
  // onChange receives the committed, trimmed duration string (on blur or Enter,
  // never on every keystroke) and only fires for a syntactically valid value
  // that differs from `value`. It may return a promise; while pending the input
  // is disabled, and the local value reverts to `value` if the promise rejects.
  onChange: (next: string) => void | Promise<void>
  placeholder?: string
  disabled?: boolean
  id?: string
  'aria-label'?: string
}

// DurationField is a text input for Go duration strings ("5m", "24h", "1h30m").
// It validates the input client-side against the same grammar the backend's
// time.ParseDuration enforces, surfacing an inline error and refusing to commit
// an invalid value rather than letting the PATCH round-trip and fail. Valid
// edits commit through the shared optimistic / disable-while-pending /
// revert-on-reject contract used by the other Settings controls.
export default function DurationField({
  value,
  onChange,
  placeholder,
  disabled,
  id,
  'aria-label': ariaLabel,
}: DurationFieldProps) {
  const [text, setText] = useState(value)
  const [pending, setPending] = useState(false)
  const [focused, setFocused] = useState(false)
  const [invalid, setInvalid] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)
  const reactId = useId()
  const errorId = `${id ?? reactId}-duration-error`

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Re-sync from props only when idle so a poll can't overwrite typing.
  useEffect(() => {
    if (!focused && !pendingRef.current) {
      setText(value)
      setInvalid(false)
    }
  }, [value, focused])

  const isDisabled = disabled || pending

  const commit = useCallback(() => {
    if (disabled || pending) return
    const trimmed = text.trim()
    // Empty input reverts to the controlled value rather than committing a clear.
    if (trimmed === '') {
      setText(value)
      setInvalid(false)
      return
    }
    if (!isValidGoDuration(trimmed)) {
      setInvalid(true)
      return
    }
    setInvalid(false)
    if (trimmed === value) {
      setText(value)
      return
    }
    pendingRef.current = true
    setPending(true)
    Promise.resolve()
      .then(() => onChange(trimmed))
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
    <div className="inline-flex flex-col items-end gap-1">
      <input
        type="text"
        inputMode="text"
        id={id}
        aria-label={ariaLabel}
        aria-invalid={invalid || undefined}
        aria-describedby={invalid ? errorId : undefined}
        value={text}
        placeholder={placeholder ?? 'e.g. 5m, 24h'}
        disabled={isDisabled}
        onChange={(e) => {
          setText(e.target.value)
          if (invalid) setInvalid(false)
        }}
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
        className={`w-28 rounded-md border bg-slate-800/60 px-2 py-1 text-right text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus-visible:ring-1 disabled:cursor-not-allowed disabled:opacity-50 ${
          invalid
            ? 'border-rose-500/70 focus:border-rose-400 focus-visible:ring-rose-400'
            : 'border-slate-700 focus:border-emerald-400 focus-visible:ring-emerald-400'
        }`}
      />
      {invalid && (
        <span id={errorId} className="text-[11px] text-rose-400">
          Invalid duration (e.g. 5m, 24h)
        </span>
      )}
    </div>
  )
}
