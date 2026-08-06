import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiPost, isUnresolvedQueued, resolveQueuedRequest } from './api'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

function queuedResponse(requestID = 'forge-1'): Response {
  return jsonResponse(
    {
      queued: true,
      request_id: requestID,
      poll_url: `/api/requests/${requestID}`,
      message: 'adding label',
    },
    { status: 202 },
  )
}

describe('apiPost — queued (202) outcome resolution', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  // The Forge-4r2n regression: the daemon accepted the command (202) but the
  // background bd write failed. The action must surface as an error, never as
  // a success.
  it('throws when the queued command later fails', async () => {
    fetchMock
      .mockResolvedValueOnce(queuedResponse())
      .mockResolvedValueOnce(
        jsonResponse({
          request_id: 'forge-1',
          state: 'error',
          message: 'bd update failed: exit status 1',
        }),
      )

    await expect(
      apiPost('/api/queue/Forge-abc1/apply-dispatch-tag', { anvil: 'forge' }),
    ).rejects.toMatchObject({ message: 'bd update failed: exit status 1' })
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(fetchMock.mock.calls[1][0]).toBe('/api/requests/forge-1')
  })

  it('resolves normally when the queued command succeeds', async () => {
    fetchMock
      .mockResolvedValueOnce(queuedResponse())
      .mockResolvedValueOnce(jsonResponse({ request_id: 'forge-1', state: 'ok' }))

    const out = await apiPost<{ queued?: boolean }>('/api/bead/Forge-abc1/close', {
      anvil: 'forge',
    })
    expect(isUnresolvedQueued(out)).toBe(false)
    expect(out.queued).toBe(true)
  })

  it('polls until the queued command leaves the pending state', async () => {
    fetchMock
      .mockResolvedValueOnce(queuedResponse())
      .mockResolvedValueOnce(jsonResponse({ request_id: 'forge-1', state: 'pending' }))
      .mockResolvedValueOnce(
        jsonResponse({ request_id: 'forge-1', state: 'error', message: 'anvil wedged' }),
      )

    await expect(apiPost('/api/bead/Forge-abc1/close', { anvil: 'forge' })).rejects.toBeInstanceOf(
      ApiError,
    )
    expect(fetchMock).toHaveBeenCalledTimes(3)
  })

  it('reports an unknown outcome as unresolved rather than success', async () => {
    // The daemon no longer holds a record (evicted from the bounded store).
    fetchMock
      .mockResolvedValueOnce(queuedResponse())
      .mockResolvedValueOnce(jsonResponse({ request_id: 'forge-1', state: 'unknown' }))

    const out = await apiPost('/api/bead/Forge-abc1/close', { anvil: 'forge' })
    expect(isUnresolvedQueued(out)).toBe(true)
  })

  it('reports a failed status lookup as unresolved, not as an action failure', async () => {
    fetchMock
      .mockResolvedValueOnce(queuedResponse())
      .mockResolvedValueOnce(new Response('boom', { status: 500 }))

    const out = await apiPost('/api/bead/Forge-abc1/close', { anvil: 'forge' })
    expect(isUnresolvedQueued(out)).toBe(true)
  })

  it('leaves a 202 without a request_id alone (legacy/other async shapes)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ turn_id: 't-1' }, { status: 202 }))

    const out = await apiPost<{ turn_id?: string }>('/api/forge/sessions/1/turn', {})
    expect(out.turn_id).toBe('t-1')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('still surfaces a synchronous 500 as an ApiError', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: 'anvil "forge" not found' }, { status: 500 }),
    )

    await expect(apiPost('/api/bead/Forge-abc1/close', { anvil: 'forge' })).rejects.toMatchObject({
      status: 500,
      message: 'anvil "forge" not found',
    })
  })
})

describe('resolveQueuedRequest', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('gives up as pending once the budget expires', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ request_id: 'forge-1', state: 'pending' }))

    // A zero budget means one lookup and then a pending verdict — which the
    // caller renders as "queued, outcome unknown", not success.
    const out = await resolveQueuedRequest('/api/requests/forge-1', 0)
    expect(out.state).toBe('pending')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
