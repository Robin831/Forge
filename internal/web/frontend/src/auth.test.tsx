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
