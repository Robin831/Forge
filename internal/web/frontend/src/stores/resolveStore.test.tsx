import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'

import {
  ResolveStoreProvider,
  resolveKey,
  useEscalation,
  useEscalations,
  useResolveActions,
  useResolveEntries,
  useResolveStatus,
} from './resolveStore'

function wrapper({ children }: { children: ReactNode }) {
  return <ResolveStoreProvider>{children}</ResolveStoreProvider>
}

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

describe('resolveKey', () => {
  it('joins anvil and bead into a stable key', () => {
    expect(resolveKey('Forge-aaaa', 'forge')).toBe('forge/Forge-aaaa')
  })

  it('folds the worker id in when supplied', () => {
    expect(resolveKey('Forge-aaaa', 'forge', 'w-1')).toBe(
      'forge/Forge-aaaa#w-1',
    )
  })
})

describe('useResolveStatus', () => {
  it('returns the IDLE entry for unknown keys', () => {
    const { result } = renderHook(() => useResolveStatus('unknown'), { wrapper })
    expect(result.current.status).toBe('idle')
    expect(result.current.error).toBeUndefined()
  })

  it('throws a helpful error when the provider is missing', () => {
    // Silence the React error overlay for the duration of this test —
    // renderHook would otherwise log the boundary error to stderr.
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      expect(() => renderHook(() => useResolveStatus('k'))).toThrow(
        /ResolveStoreProvider/,
      )
    } finally {
      spy.mockRestore()
    }
  })
})

describe('useResolveActions.run', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('transitions pending → success on a 2xx response', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useResolveStatus('forge/Forge-aaaa'),
      }),
      { wrapper },
    )
    expect(result.current.entry.status).toBe('idle')

    let success: boolean | undefined
    await act(async () => {
      success = await result.current.actions.run(
        'forge/Forge-aaaa',
        'Forge-aaaa',
        { verb: 'retry', anvil: 'forge' },
      )
    })

    expect(success).toBe(true)
    expect(result.current.entry.status).toBe('success')
    expect(result.current.entry.verb).toBe('retry')
    expect(result.current.entry.error).toBeUndefined()
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/forge/resolve',
      expect.objectContaining({ method: 'POST' }),
    )
  })

  it('transitions to error with the daemon message on a 4xx response', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: 'note is required for clarify' }, { status: 400 }),
    )

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useResolveStatus('forge/Forge-bbbb'),
      }),
      { wrapper },
    )

    let success: boolean | undefined
    await act(async () => {
      success = await result.current.actions.run(
        'forge/Forge-bbbb',
        'Forge-bbbb',
        { verb: 'clarify', anvil: 'forge' },
      )
    })

    expect(success).toBe(false)
    expect(result.current.entry.status).toBe('error')
    expect(result.current.entry.error).toBe('note is required for clarify')
    expect(result.current.entry.verb).toBe('clarify')
  })

  it('marks the entry pending while the request is in flight', async () => {
    let resolveFetch: ((value: Response) => void) | undefined
    const pending = new Promise<Response>((res) => {
      resolveFetch = res
    })
    fetchMock.mockReturnValueOnce(pending)

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useResolveStatus('forge/Forge-cccc'),
      }),
      { wrapper },
    )

    let runPromise: Promise<boolean> | undefined
    act(() => {
      runPromise = result.current.actions.run(
        'forge/Forge-cccc',
        'Forge-cccc',
        { verb: 'stop', anvil: 'forge' },
      )
    })
    expect(result.current.entry.status).toBe('pending')
    expect(result.current.entry.verb).toBe('stop')

    await act(async () => {
      resolveFetch?.(jsonResponse({ status: 'ok' }))
      await runPromise
    })
    expect(result.current.entry.status).toBe('success')
  })

  it('reset clears an entry back to idle', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entries: useResolveEntries(),
        entry: useResolveStatus('forge/Forge-dddd'),
      }),
      { wrapper },
    )

    await act(async () => {
      await result.current.actions.run('forge/Forge-dddd', 'Forge-dddd', {
        verb: 'clear',
        anvil: 'forge',
      })
    })
    expect(result.current.entry.status).toBe('success')
    expect(result.current.entries['forge/Forge-dddd']).toBeDefined()

    act(() => {
      result.current.actions.reset('forge/Forge-dddd')
    })
    expect(result.current.entry.status).toBe('idle')
    expect(result.current.entries['forge/Forge-dddd']).toBeUndefined()
  })

  it('tracks independent state per key', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ status: 'ok' }))
      .mockResolvedValueOnce(
        jsonResponse({ error: 'no such bead' }, { status: 404 }),
      )

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        first: useResolveStatus('forge/Forge-aaaa'),
        second: useResolveStatus('forge/Forge-bbbb'),
      }),
      { wrapper },
    )

    await act(async () => {
      await result.current.actions.run('forge/Forge-aaaa', 'Forge-aaaa', {
        verb: 'retry',
        anvil: 'forge',
      })
      await result.current.actions.run('forge/Forge-bbbb', 'Forge-bbbb', {
        verb: 'retry',
        anvil: 'forge',
      })
    })

    expect(result.current.first.status).toBe('success')
    expect(result.current.second.status).toBe('error')
    expect(result.current.second.error).toBe('no such bead')
  })
})

describe('useResolveActions.fetchEscalation', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  const DETAIL = {
    bead_id: 'Forge-aaaa',
    anvil: 'forge',
    branch: 'forge/Forge-aaaa',
    worktree_path: '/tmp/Forge-aaaa',
    worktree_exists: true,
    escalation_message: 'smith timed out',
    errors: [],
  }

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('starts idle and transitions to success with detail on 2xx', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useEscalation('Forge-aaaa'),
      }),
      { wrapper },
    )

    expect(result.current.entry.status).toBe('idle')
    expect(result.current.entry.data).toBeUndefined()

    let returned: unknown
    await act(async () => {
      returned = await result.current.actions.fetchEscalation('Forge-aaaa')
    })

    expect(returned).toEqual(DETAIL)
    expect(result.current.entry.status).toBe('success')
    expect(result.current.entry.data).toEqual(DETAIL)
    expect(result.current.entry.error).toBeUndefined()
  })

  it('transitions to error and surfaces the message on a non-2xx response', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: 'not found' }, { status: 404 }),
    )

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useEscalation('Forge-bbbb'),
      }),
      { wrapper },
    )

    let returned: unknown
    await act(async () => {
      returned = await result.current.actions.fetchEscalation('Forge-bbbb')
    })

    expect(returned).toBeNull()
    expect(result.current.entry.status).toBe('error')
    expect(result.current.entry.error).toBe('not found')
    expect(result.current.entry.data).toBeUndefined()
  })

  it('sets status to loading while the request is in flight', async () => {
    let resolveFetch: ((value: Response) => void) | undefined
    const pending = new Promise<Response>((res) => {
      resolveFetch = res
    })
    fetchMock.mockReturnValueOnce(pending)

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useEscalation('Forge-cccc'),
      }),
      { wrapper },
    )

    let fetchPromise: Promise<unknown> | undefined
    act(() => {
      fetchPromise = result.current.actions.fetchEscalation('Forge-cccc')
    })
    expect(result.current.entry.status).toBe('loading')

    await act(async () => {
      resolveFetch?.(jsonResponse(DETAIL))
      await fetchPromise
    })
    expect(result.current.entry.status).toBe('success')
  })

  it('preserves prior data on refetch so consumers do not flash a skeleton', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(jsonResponse(DETAIL))

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useEscalation('Forge-aaaa'),
      }),
      { wrapper },
    )

    // First fetch — populate data.
    await act(async () => {
      await result.current.actions.fetchEscalation('Forge-aaaa')
    })
    expect(result.current.entry.data).toEqual(DETAIL)

    // Second fetch — during loading the prior data must still be present.
    let refetchPromise: Promise<unknown> | undefined
    let resolveFetch2: ((value: Response) => void) | undefined
    const pending2 = new Promise<Response>((res) => {
      resolveFetch2 = res
    })
    fetchMock.mockReturnValueOnce(pending2)

    act(() => {
      refetchPromise = result.current.actions.fetchEscalation('Forge-aaaa')
    })
    expect(result.current.entry.status).toBe('loading')
    expect(result.current.entry.data).toEqual(DETAIL)

    await act(async () => {
      resolveFetch2?.(jsonResponse(DETAIL))
      await refetchPromise
    })
    expect(result.current.entry.status).toBe('success')
  })

  it('preserves prior data when a refetch errors', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        entry: useEscalation('Forge-dddd'),
      }),
      { wrapper },
    )

    // Successful first fetch.
    await act(async () => {
      await result.current.actions.fetchEscalation('Forge-dddd')
    })
    expect(result.current.entry.data).toEqual(DETAIL)

    // Failing refetch — data must be preserved.
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: 'server error' }, { status: 500 }),
    )
    await act(async () => {
      await result.current.actions.fetchEscalation('Forge-dddd')
    })

    expect(result.current.entry.status).toBe('error')
    expect(result.current.entry.error).toBe('server error')
    expect(result.current.entry.data).toEqual(DETAIL)
  })

  it('tracks independent escalation state per id', async () => {
    const DETAIL_B = { ...DETAIL, bead_id: 'Forge-eeee', anvil: 'other' }
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(jsonResponse(DETAIL_B))

    const { result } = renderHook(
      () => ({
        actions: useResolveActions(),
        escalations: useEscalations(),
      }),
      { wrapper },
    )

    await act(async () => {
      await result.current.actions.fetchEscalation('Forge-aaaa')
      await result.current.actions.fetchEscalation('Forge-eeee')
    })

    expect(result.current.escalations['Forge-aaaa'].status).toBe('success')
    expect(result.current.escalations['Forge-aaaa'].data?.anvil).toBe('forge')
    expect(result.current.escalations['Forge-eeee'].status).toBe('success')
    expect(result.current.escalations['Forge-eeee'].data?.anvil).toBe('other')
  })
})
