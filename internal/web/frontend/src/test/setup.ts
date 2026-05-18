import '@testing-library/jest-dom/vitest'

// react-router 7 creates AbortControllers that produce jsdom's AbortSignal, but
// globalThis.Request is Node's native Request (undici) which validates
// `signal instanceof AbortSignal` against the native class — a different class
// from jsdom's. Strip signals from Request init so the native constructor
// doesn't see them. Navigation abort isn't exercised by these tests so this
// is safe; the rest of the RequestInit is forwarded unchanged.
const _OrigRequest = globalThis.Request
if (typeof _OrigRequest !== 'undefined') {
  globalThis.Request = new Proxy(_OrigRequest, {
    construct(Target, [input, init]: [RequestInfo | URL, RequestInit?]) {
      if (init?.signal != null) {
        const { signal: _dropped, ...safeInit } = init
        return Reflect.construct(Target, [input, safeInit])
      }
      return Reflect.construct(Target, [input, init])
    },
  }) as unknown as typeof Request
}
