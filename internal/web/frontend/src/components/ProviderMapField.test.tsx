import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ProviderMapField, { PROVIDER_STAGES } from './ProviderMapField'

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

describe('ProviderMapField', () => {
  it('renders one chain editor per stage', () => {
    render(
      <ProviderMapField
        value={{ smith: ['claude'] }}
        onChange={() => {}}
        aria-label="Stage providers"
      />,
    )
    for (const stage of PROVIDER_STAGES) {
      expect(
        screen.getByRole('textbox', { name: `Add provider for ${stage}` }),
      ).toBeInTheDocument()
    }
    // The existing smith chain renders its provider.
    expect(screen.getByText('claude')).toBeInTheDocument()
  })

  it('honors a custom stage set', () => {
    render(
      <ProviderMapField
        value={{}}
        onChange={() => {}}
        stages={['smith', 'warden']}
        aria-label="Stage providers"
      />,
    )
    expect(
      screen.getByRole('textbox', { name: 'Add provider for smith' }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('textbox', { name: 'Add provider for schematic' }),
    ).not.toBeInTheDocument()
  })

  it('adds a provider to a stage and emits the full map', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ProviderMapField
        value={{ smith: ['claude'] }}
        onChange={onChange}
        aria-label="Stage providers"
      />,
    )

    await userEvent.type(
      screen.getByRole('textbox', { name: 'Add provider for warden' }),
      'gemini{Enter}',
    )
    expect(onChange).toHaveBeenCalledWith({
      smith: ['claude'],
      warden: ['gemini'],
    })
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('omits a stage from the map when its last provider is removed', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ProviderMapField
        value={{ smith: ['claude'], warden: ['gemini'] }}
        onChange={onChange}
        aria-label="Stage providers"
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Remove gemini' }))
    expect(onChange).toHaveBeenCalledWith({ smith: ['claude'] })
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('reorders providers within a stage', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ProviderMapField
        value={{ smith: ['claude', 'gemini'] }}
        onChange={onChange}
        aria-label="Stage providers"
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Move claude down' }))
    expect(onChange).toHaveBeenCalledWith({ smith: ['gemini', 'claude'] })
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('reverts the optimistic map when onChange rejects', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ProviderMapField
        value={{ smith: ['claude'] }}
        onChange={onChange}
        aria-label="Stage providers"
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Remove claude' }))
    expect(screen.queryByText('claude')).not.toBeInTheDocument()

    await act(async () => {
      d.reject(new Error('fail'))
      try {
        await d.promise
      } catch {
        /* expected */
      }
    })
    expect(screen.getByText('claude')).toBeInTheDocument()
  })

  it('renders an inherit placeholder and overrides to an empty map', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ProviderMapField
        value={null}
        onChange={onChange}
        inheritable
        aria-label="Stage providers"
      />,
    )

    expect(screen.getByText(/inherits global/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Override' }))
    expect(onChange).toHaveBeenCalledWith({})
    await act(() => {
      d.resolve()
      return d.promise
    })
  })

  it('resets to inherit via the Inherit button', async () => {
    const d = deferred()
    const onChange = vi.fn(() => d.promise)
    render(
      <ProviderMapField
        value={{ smith: ['claude'] }}
        onChange={onChange}
        inheritable
        aria-label="Stage providers"
      />,
    )

    await userEvent.click(
      screen.getByRole('button', { name: 'Reset Stage providers to inherit' }),
    )
    expect(onChange).toHaveBeenCalledWith(null)
    await act(() => {
      d.resolve()
      return d.promise
    })
  })
})
