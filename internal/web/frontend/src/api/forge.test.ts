import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  RESOLVE_VERBS,
  fetchEscalation,
  postResolve,
  type EscalationDetail,
  type ResolveVerb,
} from './forge'
import { ApiError } from '../api'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

describe('RESOLVE_VERBS', () => {
  it('lists every backend verb in canonical order', () => {
    expect(RESOLVE_VERBS).toEqual([
      'clear',
      'retry',
      'clarify',
      'unclarify',
      'stop',
      'approve-as-is',
      'warden-rerun',
    ])
  })

  it('compiles each entry as a ResolveVerb (type-level)', () => {
    // The assignment fails at compile time if the const-tuple drifts from
    // the ResolveVerb union — keeping this test guards against a stray
    // verb being added to one place but not the other.
    const sample: ResolveVerb = RESOLVE_VERBS[0]
    expect(sample).toBe('clear')
  })
})

describe('fetchEscalation', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('issues a GET to the bead-scoped path with credentials', async () => {
    const payload: EscalationDetail = {
      bead_id: 'Forge-aaaa',
      anvil: 'forge',
      worktree_exists: false,
      escalation_message: 'temper failed',
    }
    fetchMock.mockResolvedValueOnce(jsonResponse(payload))

    const out = await fetchEscalation('Forge-aaaa')

    expect(out).toEqual(payload)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/forge/escalation/Forge-aaaa')
    expect(init.credentials).toBe('include')
  })

  it('appends the anvil hint as a query string when provided', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        bead_id: 'Forge-aaaa',
        anvil: 'metadata',
        worktree_exists: false,
        escalation_message: '',
      }),
    )

    await fetchEscalation('Forge-aaaa', 'metadata')

    const [url] = fetchMock.mock.calls[0] as [string]
    expect(url).toBe('/api/forge/escalation/Forge-aaaa?anvil=metadata')
  })

  it('percent-encodes path-unsafe bead identifiers', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        bead_id: 'ext-org/repo#42',
        anvil: 'forge',
        worktree_exists: false,
        escalation_message: '',
      }),
    )

    await fetchEscalation('ext-org/repo#42')

    const [url] = fetchMock.mock.calls[0] as [string]
    expect(url).toBe('/api/forge/escalation/ext-org%2Frepo%2342')
  })

  it('surfaces non-2xx responses as ApiError with the daemon message', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: 'invalid bead id' }, { status: 400 }),
    )

    const rejection = fetchEscalation('bad')
    await expect(rejection).rejects.toMatchObject({
      status: 400,
      message: 'invalid bead id',
    })
    await expect(rejection).rejects.toBeInstanceOf(ApiError)
  })
})

describe('postResolve', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('POSTs with the X-Forge-Action header and snake_case body', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const out = await postResolve('Forge-aaaa', {
      verb: 'retry',
      anvil: 'forge',
      note: 'manual nudge',
    })

    expect(out).toEqual({ status: 'ok' })
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/forge/resolve')
    expect(init.method).toBe('POST')
    const headers = init.headers as Record<string, string>
    expect(headers['X-Forge-Action']).toBe('1')
    expect(headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body as string)).toEqual({
      bead_id: 'Forge-aaaa',
      action: 'retry',
      anvil_name: 'forge',
      note: 'manual nudge',
    })
  })

  it('threads forgeId through as the snake_case forge_id field', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: 'queued' }, { status: 202 }))

    await postResolve('Forge-bbbb', {
      verb: 'stop',
      anvil: 'forge',
      forgeId: 'forge-prod',
    })

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(JSON.parse(init.body as string)).toEqual({
      bead_id: 'Forge-bbbb',
      action: 'stop',
      anvil_name: 'forge',
      forge_id: 'forge-prod',
    })
  })

  it('surfaces daemon errors as ApiError', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: 'note is required for clarify' }, { status: 400 }),
    )

    await expect(
      postResolve('Forge-cccc', { verb: 'clarify', anvil: 'forge' }),
    ).rejects.toMatchObject({
      status: 400,
      message: 'note is required for clarify',
    })
  })
})
