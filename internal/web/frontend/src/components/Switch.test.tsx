import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Switch from './Switch'

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

describe('Switch', () => {
  it('renders unchecked state', () => {
    render(<Switch checked={false} onChange={() => {}} aria-label="Toggle" />)
    const sw = screen.getByRole('switch')
    expect(sw).toHaveAttribute('aria-checked', 'false')
  })

  it('renders checked state', () => {
    render(<Switch checked={true} onChange={() => {}} aria-label="Toggle" />)
    const sw = screen.getByRole('switch')
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })

  it('uses aria-checked, not aria-pressed, for switch semantics', () => {
    render(<Switch checked={true} onChange={() => {}} aria-label="Toggle" />)
    const sw = screen.getByRole('switch')
    expect(sw).toHaveAttribute('aria-checked')
    expect(sw).not.toHaveAttribute('aria-pressed')
  })

  it('flips optimistically on click', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(<Switch checked={false} onChange={onChange} aria-label="Toggle" />)
    const sw = screen.getByRole('switch')

    await userEvent.click(sw)

    expect(sw).toHaveAttribute('aria-checked', 'true')
    expect(onChange).toHaveBeenCalledWith(true)

    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('disables while onChange is pending', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(<Switch checked={false} onChange={onChange} aria-label="Toggle" />)
    const sw = screen.getByRole('switch')

    await userEvent.click(sw)
    expect(sw).toBeDisabled()

    await act(() => {
      d.resolve()
      return d.promise
    })
    expect(sw).not.toBeDisabled()
  })

  it('reverts optimistic value on rejection', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(<Switch checked={false} onChange={onChange} aria-label="Toggle" />)
    const sw = screen.getByRole('switch')

    await userEvent.click(sw)
    expect(sw).toHaveAttribute('aria-checked', 'true')

    await act(async () => {
      d.reject(new Error('fail'))
      try { await d.promise } catch { /* expected */ }
    })
    expect(sw).toHaveAttribute('aria-checked', 'false')
  })

  it('does not toggle when disabled', async () => {
    const onChange = vi.fn()
    render(
      <Switch checked={false} onChange={onChange} disabled aria-label="Toggle" />,
    )
    const sw = screen.getByRole('switch')
    expect(sw).toBeDisabled()

    await userEvent.click(sw)
    expect(onChange).not.toHaveBeenCalled()
  })

  it('activates via keyboard (space/enter on native button)', async () => {
    const onChange = vi.fn()
    render(<Switch checked={false} onChange={onChange} aria-label="Toggle" />)

    const sw = screen.getByRole('switch')
    sw.focus()
    await userEvent.keyboard('{Enter}')
    expect(onChange).toHaveBeenCalledTimes(1)
  })

  it('syncs optimistic to checked when prop changes while idle', () => {
    const { rerender } = render(
      <Switch checked={false} onChange={() => {}} aria-label="Toggle" />,
    )
    const sw = screen.getByRole('switch')
    expect(sw).toHaveAttribute('aria-checked', 'false')

    rerender(<Switch checked={true} onChange={() => {}} aria-label="Toggle" />)
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })

  it('does not flash stale checked value when pending settles', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    const { rerender } = render(
      <Switch checked={false} onChange={onChange} aria-label="Toggle" />,
    )
    const sw = screen.getByRole('switch')

    await userEvent.click(sw)
    expect(sw).toHaveAttribute('aria-checked', 'true')

    // Settle the promise — parent checked is still false (hasn't polled yet).
    await act(() => {
      d.resolve()
      return d.promise
    })

    // The optimistic value should stay true (not flash back to false).
    expect(sw).toHaveAttribute('aria-checked', 'true')

    // Later the parent polls and confirms the new value.
    rerender(<Switch checked={true} onChange={onChange} aria-label="Toggle" />)
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })

  it('forwards id and aria-label', () => {
    render(
      <Switch
        checked={false}
        onChange={() => {}}
        id="my-switch"
        aria-label="My toggle"
      />,
    )
    const sw = screen.getByRole('switch')
    expect(sw).toHaveAttribute('id', 'my-switch')
    expect(sw).toHaveAttribute('aria-label', 'My toggle')
  })
})
