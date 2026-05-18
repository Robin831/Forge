import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'

const { useAuthMock } = vi.hoisted(() => {
  type State = {
    authenticated: boolean
    user: string | null
    loading: boolean
    login: (
      user: string,
      password: string,
    ) => Promise<{ ok: boolean; error?: string }>
    logout: () => Promise<void>
    refresh: () => Promise<void>
  }
  return {
    useAuthMock: vi.fn(
      (): State => ({
        authenticated: true,
        user: 'alice',
        loading: false,
        login: async () => ({ ok: true }),
        logout: async () => {},
        refresh: async () => {},
      }),
    ),
  }
})

vi.mock('../auth', () => ({
  useAuth: () => useAuthMock(),
}))

import { KEY_PREFIX, clearAll, useUIState } from './useUIState'

function wrapperFor(path = '/dashboard') {
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={[path]}>{children}</MemoryRouter>
  )
}

beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
  useAuthMock.mockReturnValue({
    authenticated: true,
    user: 'alice',
    loading: false,
    login: async () => ({ ok: true }),
    logout: async () => {},
    refresh: async () => {},
  })
})

afterEach(() => {
  vi.useRealTimers()
})

describe('useUIState', () => {
  it('returns the initial value when storage is empty', () => {
    const { result } = renderHook(() => useUIState('sort', 'asc'), {
      wrapper: wrapperFor('/dashboard'),
    })
    expect(result.current[0]).toBe('asc')
  })

  it('reads an existing JSON value from sessionStorage on mount', () => {
    sessionStorage.setItem(`${KEY_PREFIX}dashboard.sort`, JSON.stringify('desc'))
    const { result } = renderHook(() => useUIState('sort', 'asc'), {
      wrapper: wrapperFor('/dashboard'),
    })
    expect(result.current[0]).toBe('desc')
  })

  it('writes to sessionStorage after the debounce window', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useUIState('sort', 'asc'), {
      wrapper: wrapperFor('/dashboard'),
    })

    act(() => {
      result.current[1]('desc')
    })
    expect(result.current[0]).toBe('desc')
    // Write should not have happened yet.
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.sort`)).toBeNull()

    act(() => {
      vi.advanceTimersByTime(150)
    })
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.sort`)).toBe(
      JSON.stringify('desc'),
    )
  })

  it('coalesces rapid setter calls into a single debounced write', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useUIState('counter', 0), {
      wrapper: wrapperFor('/dashboard'),
    })

    act(() => {
      result.current[1](1)
      vi.advanceTimersByTime(50)
      result.current[1](2)
      vi.advanceTimersByTime(50)
      result.current[1](3)
    })
    // Still within the debounce window of the last call — no write yet.
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.counter`)).toBeNull()

    act(() => {
      vi.advanceTimersByTime(150)
    })
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.counter`)).toBe(
      JSON.stringify(3),
    )
  })

  it('round-trips JSON-serializable objects', () => {
    vi.useFakeTimers()
    type Filters = { tags: string[]; open: boolean }
    const { result, unmount } = renderHook(
      () => useUIState<Filters>('filters', { tags: [], open: false }),
      { wrapper: wrapperFor('/queue') },
    )

    act(() => {
      result.current[1]({ tags: ['ready', 'p0'], open: true })
      vi.advanceTimersByTime(150)
    })

    unmount()

    const { result: next } = renderHook(
      () => useUIState<Filters>('filters', { tags: [], open: false }),
      { wrapper: wrapperFor('/queue') },
    )
    expect(next.current[0]).toEqual({ tags: ['ready', 'p0'], open: true })
  })

  it('supports functional updaters', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useUIState<number>('n', 0), {
      wrapper: wrapperFor('/dashboard'),
    })
    act(() => {
      result.current[1]((prev) => prev + 1)
      result.current[1]((prev) => prev + 1)
      vi.advanceTimersByTime(150)
    })
    expect(result.current[0]).toBe(2)
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.n`)).toBe(
      JSON.stringify(2),
    )
  })

  it('falls back to the initial value when stored JSON is malformed', () => {
    sessionStorage.setItem(`${KEY_PREFIX}dashboard.sort`, '{not-json')
    const { result } = renderHook(() => useUIState('sort', 'asc'), {
      wrapper: wrapperFor('/dashboard'),
    })
    expect(result.current[0]).toBe('asc')
  })

  it('uses localStorage when storage: "local" is requested', () => {
    vi.useFakeTimers()
    const { result } = renderHook(
      () => useUIState('theme', 'dark', { storage: 'local' }),
      { wrapper: wrapperFor('/dashboard') },
    )
    act(() => {
      result.current[1]('light')
      vi.advanceTimersByTime(150)
    })
    expect(localStorage.getItem(`${KEY_PREFIX}dashboard.theme`)).toBe(
      JSON.stringify('light'),
    )
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.theme`)).toBeNull()
  })

  it('reads from localStorage when storage: "local"', () => {
    localStorage.setItem(`${KEY_PREFIX}dashboard.theme`, JSON.stringify('light'))
    const { result } = renderHook(
      () => useUIState('theme', 'dark', { storage: 'local' }),
      { wrapper: wrapperFor('/dashboard') },
    )
    expect(result.current[0]).toBe('light')
  })

  it('namespaces keys by route when scope: "route" (default)', () => {
    vi.useFakeTimers()
    const a = renderHook(() => useUIState('expanded', false), {
      wrapper: wrapperFor('/queue'),
    })
    const b = renderHook(() => useUIState('expanded', false), {
      wrapper: wrapperFor('/ingots'),
    })

    act(() => {
      a.result.current[1](true)
      vi.advanceTimersByTime(150)
    })

    expect(sessionStorage.getItem(`${KEY_PREFIX}queue.expanded`)).toBe(
      JSON.stringify(true),
    )
    expect(b.result.current[0]).toBe(false)
    expect(sessionStorage.getItem(`${KEY_PREFIX}ingots.expanded`)).toBeNull()
  })

  it('namespaces keys by user when scope: "user"', () => {
    vi.useFakeTimers()
    const { result } = renderHook(
      () => useUIState('prefs', 'a', { scope: 'user', storage: 'local' }),
      { wrapper: wrapperFor('/dashboard') },
    )
    act(() => {
      result.current[1]('b')
      vi.advanceTimersByTime(150)
    })
    expect(localStorage.getItem(`${KEY_PREFIX}alice.prefs`)).toBe(
      JSON.stringify('b'),
    )
  })

  it('falls back to "anon" when no user is authenticated', () => {
    vi.useFakeTimers()
    useAuthMock.mockReturnValue({
      authenticated: false,
      user: null,
      loading: false,
      login: async () => ({ ok: false }),
      logout: async () => {},
      refresh: async () => {},
    })
    const { result } = renderHook(
      () => useUIState('prefs', 'a', { scope: 'user', storage: 'local' }),
      { wrapper: wrapperFor('/dashboard') },
    )
    act(() => {
      result.current[1]('b')
      vi.advanceTimersByTime(150)
    })
    expect(localStorage.getItem(`${KEY_PREFIX}anon.prefs`)).toBe(
      JSON.stringify('b'),
    )
  })

  it('flushes a pending write on unmount', () => {
    vi.useFakeTimers()
    const { result, unmount } = renderHook(() => useUIState('sort', 'asc'), {
      wrapper: wrapperFor('/dashboard'),
    })
    act(() => {
      result.current[1]('desc')
    })
    // Unmount before debounce expires.
    unmount()
    expect(sessionStorage.getItem(`${KEY_PREFIX}dashboard.sort`)).toBe(
      JSON.stringify('desc'),
    )
  })
})

describe('clearAll', () => {
  it('removes only forge.ui.* keys from both storages', () => {
    sessionStorage.setItem(`${KEY_PREFIX}a.k1`, '"x"')
    sessionStorage.setItem('unrelated', 'keep')
    localStorage.setItem(`${KEY_PREFIX}b.k2`, '"y"')
    localStorage.setItem('also-unrelated', 'keep')

    clearAll()

    expect(sessionStorage.getItem(`${KEY_PREFIX}a.k1`)).toBeNull()
    expect(localStorage.getItem(`${KEY_PREFIX}b.k2`)).toBeNull()
    expect(sessionStorage.getItem('unrelated')).toBe('keep')
    expect(localStorage.getItem('also-unrelated')).toBe('keep')
  })

  it('honours a custom prefix', () => {
    sessionStorage.setItem(`${KEY_PREFIX}a.k1`, '"x"')
    sessionStorage.setItem(`${KEY_PREFIX}b.k2`, '"y"')
    clearAll(`${KEY_PREFIX}a.`)
    expect(sessionStorage.getItem(`${KEY_PREFIX}a.k1`)).toBeNull()
    expect(sessionStorage.getItem(`${KEY_PREFIX}b.k2`)).toBe('"y"')
  })
})
