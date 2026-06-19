import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import DurationField, { isValidGoDuration } from './DurationField'

afterEach(cleanup)

function deferred<T = void>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('isValidGoDuration', () => {
  it.each([
    '5m',
    '24h',
    '300ms',
    '1h30m',
    '2h45m',
    '-1.5h',
    '+5s',
    '0',
    '-0',
    '500us',
    '500µs',
    '500μs',
    '10ns',
    '1.5h',
    '5m0s',
    '168h0m0s',
  ])('accepts %s', (s) => {
    expect(isValidGoDuration(s)).toBe(true)
  })

  it.each([
    '',
    '5',
    'm',
    'abc',
    '5x',
    '1.2.3h',
    '.h',
    '5 m',
    '5min',
    '-',
    '+',
    '1h2',
  ])('rejects %s', (s) => {
    expect(isValidGoDuration(s)).toBe(false)
  })
})

describe('DurationField', () => {
  it('renders the current value', () => {
    render(<DurationField value="5m0s" onChange={() => {}} aria-label="Poll interval" />)
    expect(screen.getByRole('textbox', { name: 'Poll interval' })).toHaveValue('5m0s')
  })

  it('commits a valid duration on blur', async () => {
    const user = userEvent.setup()
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(<DurationField value="5m0s" onChange={onChange} aria-label="Poll interval" />)

    const input = screen.getByRole('textbox', { name: 'Poll interval' })
    await user.clear(input)
    await user.type(input, '10m')
    await user.tab()

    expect(onChange).toHaveBeenCalledWith('10m')
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('does not commit an unchanged value', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DurationField value="5m0s" onChange={onChange} aria-label="Poll interval" />)

    const input = screen.getByRole('textbox', { name: 'Poll interval' })
    await user.click(input)
    await user.tab()

    expect(onChange).not.toHaveBeenCalled()
  })

  it('flags an invalid duration and refuses to commit', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DurationField value="5m0s" onChange={onChange} aria-label="Poll interval" />)

    const input = screen.getByRole('textbox', { name: 'Poll interval' })
    await user.clear(input)
    await user.type(input, 'nonsense')
    await user.tab()

    expect(onChange).not.toHaveBeenCalled()
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText(/Invalid duration/i)).toBeInTheDocument()
  })

  it('clears the invalid flag once the user edits again', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DurationField value="5m0s" onChange={onChange} aria-label="Poll interval" />)

    const input = screen.getByRole('textbox', { name: 'Poll interval' })
    await user.clear(input)
    await user.type(input, 'bad')
    await user.tab()
    expect(input).toHaveAttribute('aria-invalid', 'true')

    await user.type(input, '1h')
    expect(input).not.toHaveAttribute('aria-invalid')
  })

  it('reverts to the controlled value on empty input', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<DurationField value="5m0s" onChange={onChange} aria-label="Poll interval" />)

    const input = screen.getByRole('textbox', { name: 'Poll interval' })
    await user.clear(input)
    await user.tab()

    expect(onChange).not.toHaveBeenCalled()
    expect(input).toHaveValue('5m0s')
  })

  it('reverts to the controlled value when onChange rejects', async () => {
    const user = userEvent.setup()
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(<DurationField value="5m0s" onChange={onChange} aria-label="Poll interval" />)

    const input = screen.getByRole('textbox', { name: 'Poll interval' })
    await user.clear(input)
    await user.type(input, '10m')
    await user.tab()
    expect(onChange).toHaveBeenCalledWith('10m')

    await act(async () => {
      d.reject(new Error('boom'))
      await Promise.resolve()
    })
    expect(input).toHaveValue('5m0s')
  })
})
