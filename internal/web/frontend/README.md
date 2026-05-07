# Hearth 2.0 frontend

React 19 + Vite 7 + Tailwind 4 dashboard for the Forge daemon. The build output
is committed at `../dist/` and embedded into the `forge` binary via
`//go:embed dist` in [internal/web/embed.go](../embed.go), so a single
container ships both the daemon and the UI.

## Stack

- React 19 with TypeScript 5.x
- Vite 7 (with `@vitejs/plugin-react` 5.x)
- Tailwind 4 via `@tailwindcss/vite` (no PostCSS config needed)
- React Router 7
- `lucide-react` for icons

No state management library, SWR, or react-query — a single `useApiPoll`
hook covers all three dashboard panes.

## Develop

The Vite dev server proxies `/api`, `/login`, `/logout`, and `/healthz` to the
Go daemon on `localhost:8080`. Start the daemon with `FORGE_WEB_ENABLED=1` (and
`FORGE_USERS=alice:<bcrypt-hash>`) in one terminal, then:

```bash
cd internal/web/frontend
npm install
npm run dev
```

## Build & embed

```bash
cd internal/web/frontend
npm run build       # writes ../dist/index.html and ../dist/assets/*
cd ../../..
go build -o forge ./cmd/forge
```

The bundle in `internal/web/dist/` is intentionally checked into the repo so
`go build` works for anyone — no separate frontend build step is required when
just compiling the binary.

## Routes

| Path        | Render                                                          |
| ----------- | --------------------------------------------------------------- |
| `/login`    | Username/password form. Redirects to `/` when already signed in |
| `/`         | Dashboard (queue / workers / events panes, polled every 5s)     |
| `*`         | Falls back to `/`                                               |

Any path the SPA does not handle and that does not exist in the embedded
filesystem is served `index.html` so client-side routing can take over.
