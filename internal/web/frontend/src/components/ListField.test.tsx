import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ListField from './ListField'

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

describe('ListField', () => {
  it('renders the current items', () => {
    render(
      <ListField
        value={['claude', 'gemini']}
        onChange={() => {}}
        aria-label="Providers"
      />,
    )
    expect(screen.getByText('claude')).toBeInTheDocument()
    expect(screen.getByText('gemini')).toBeInTheDocument()
  })

  it('treats a null value as an empty list when not inheritable', () => {
    render(<ListField value={null} onChange={() => {}} aria-label="Providers" />)
    // No items rendered, but the add controls are present.
    expect(screen.getByRole('textbox')).toBeInTheDocument()
  })

  it('appends a trimmed item on add', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ListField
        value={['claude']}
        onChange={onChange}
        addLabel="Add provider"
        aria-label="Providers"
      />,
    )

    await userEvent.type(screen.getByRole('textbox', { name: 'Add provider' }), '  gemini  ')
    await userEvent.click(screen.getByRole('button', { name: 'Add provider' }))

    expect(onChange).toHaveBeenCalledWith(['claude', 'gemini'])
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('ignores empty and duplicate adds', async () => {
    const onChange = vi.fn()
    render(
      <ListField
        value={['claude']}
        onChange={onChange}
        addLabel="Add provider"
        aria-label="Providers"
      />,
    )

    const input = screen.getByRole('textbox', { name: 'Add provider' })
    // Duplicate.
    await userEvent.type(input, 'claude{Enter}')
    expect(onChange).not.toHaveBeenCalled()
  })

  it('removes an item', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ListField
        value={['claude', 'gemini']}
        onChange={onChange}
        aria-label="Providers"
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Remove claude' }))
    expect(onChange).toHaveBeenCalledWith(['gemini'])
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('reorders an item with the move-down control', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ListField
        value={['claude', 'gemini']}
        onChange={onChange}
        aria-label="Providers"
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Move claude down' }))
    expect(onChange).toHaveBeenCalledWith(['gemini', 'claude'])
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('disables the move-up control on the first row', () => {
    render(
      <ListField
        value={['claude', 'gemini']}
        onChange={() => {}}
        aria-label="Providers"
      />,
    )
    expect(screen.getByRole('button', { name: 'Move claude up' })).toBeDisabled()
    expect(
      screen.getByRole('button', { name: 'Move gemini down' }),
    ).toBeDisabled()
  })

  it('reverts the optimistic list when onChange rejects', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ListField
        value={['claude']}
        onChange={onChange}
        aria-label="Providers"
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Remove claude' }))
    // Optimistically gone.
    expect(screen.queryByText('claude')).not.toBeInTheDocument()

    await act(async () => {
      d.reject(new Error('fail'))
      try {
        await d.promise
      } catch {
        /* expected */
      }
    })
    // Reverted.
    expect(screen.getByText('claude')).toBeInTheDocument()
  })

  it('renders an inherit placeholder and overrides to an empty list', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ListField
        value={null}
        onChange={onChange}
        inheritable
        aria-label="Providers"
      />,
    )

    expect(screen.getByText(/inherits global/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Override' }))
    expect(onChange).toHaveBeenCalledWith([])
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('resets to inherit via the Inherit button', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ListField
        value={['claude']}
        onChange={onChange}
        inheritable
        aria-label="Providers"
      />,
    )

    await userEvent.click(
      screen.getByRole('button', { name: 'Reset Providers to inherit' }),
    )
    expect(onChange).toHaveBeenCalledWith(null)
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('does not show an Inherit reset when not inheritable', () => {
    render(
      <ListField
        value={['claude']}
        onChange={() => {}}
        aria-label="Providers"
      />,
    )
    expect(
      screen.queryByRole('button', { name: /to inherit/ }),
    ).not.toBeInTheDocument()
  })
})
