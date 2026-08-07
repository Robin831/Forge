import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { AuthProvider, useAuth } from './auth'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

function wrapper({ children }: { children: ReactNode }) {
  return <AuthProvider>{children}</AuthProvider>
}

describe('AuthProvider CSRF headers', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('sends X-Forge-Action on login', async () => {
    // Initial auth-status probe on mount, then the login POST.
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))
      .mockResolvedValueOnce(jsonResponse({ authenticated: true, user: 'alice' }))

    const { result } = renderHook(() => useAuth(), { wrapper })
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      await result.current.login('alice', 'hunter2')
    })

    const loginCall = fetchMock.mock.calls.find(([url]) => url === '/login')
    expect(loginCall).toBeTruthy()
    const init = loginCall![1] as RequestInit
    expect(init.method).toBe('POST')
    expect(init.headers).toMatchObject({ 'X-Forge-Action': '1' })
  })

  it('sends X-Forge-Action on logout', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ authenticated: true, user: 'alice' }))
      .mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const { result } = renderHook(() => useAuth(), { wrapper })
    await waitFor(() => expect(result.current.loading).toBe(false))

    await act(async () => {
      await result.current.logout()
    })

    const logoutCall = fetchMock.mock.calls.find(([url]) => url === '/logout')
    expect(logoutCall).toBeTruthy()
    const init = logoutCall![1] as RequestInit
    expect(init.method).toBe('POST')
    expect(init.headers).toMatchObject({ 'X-Forge-Action': '1' })
  })
})

// Returning to a preview after signing in. The preview proxy bounces an
// unauthenticated navigation to /login?next=<preview URL>; the parameter rides
// along on the POST and the server — which alone knows preview_proxy_base —
// answers with the URL to follow. The client validates nothing and invents
// nothing, so a crafted `next` cannot turn this into an open redirect.
describe('AuthProvider login next', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  const previewURL = 'https://forge-abc1.preview.example.com/orders'

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('forwards next and returns the redirect the server validated', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))
      .mockResolvedValueOnce(
        jsonResponse({ authenticated: true, user: 'alice', redirect: previewURL }),
      )

    const { result } = renderHook(() => useAuth(), { wrapper })
    await waitFor(() => expect(result.current.loading).toBe(false))

    let outcome: { ok: boolean; redirect?: string } | undefined
    await act(async () => {
      outcome = await result.current.login('alice', 'hunter2', previewURL)
    })

    const loginCall = fetchMock.mock.calls.find(([url]) => url === '/login')
    const body = (loginCall![1] as RequestInit).body as string
    expect(new URLSearchParams(body).get('next')).toBe(previewURL)
    expect(outcome).toMatchObject({ ok: true, redirect: previewURL })
  })

  it('omits next when there is none, and reports no redirect', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))
      .mockResolvedValueOnce(jsonResponse({ authenticated: true, user: 'alice' }))

    const { result } = renderHook(() => useAuth(), { wrapper })
    await waitFor(() => expect(result.current.loading).toBe(false))

    let outcome: { ok: boolean; redirect?: string } | undefined
    await act(async () => {
      outcome = await result.current.login('alice', 'hunter2')
    })

    const loginCall = fetchMock.mock.calls.find(([url]) => url === '/login')
    const body = (loginCall![1] as RequestInit).body as string
    expect(new URLSearchParams(body).has('next')).toBe(false)
    expect(outcome?.redirect).toBeUndefined()
  })

  // A server that answers with a redirect the client did not ask for is still
  // only ever followed by LoginPage, never here: the hook reports it and the
  // page decides. This asserts the reporting half stays a plain pass-through.
  it('does not invent a redirect when the server omits one', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))
      .mockResolvedValueOnce(jsonResponse({ authenticated: true, user: 'alice' }))

    const { result } = renderHook(() => useAuth(), { wrapper })
    await waitFor(() => expect(result.current.loading).toBe(false))

    let outcome: { ok: boolean; redirect?: string } | undefined
    await act(async () => {
      outcome = await result.current.login('alice', 'hunter2', previewURL)
    })

    expect(outcome?.redirect).toBeUndefined()
  })
})
