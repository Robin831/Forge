import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))

// The build output is committed at internal/web/dist (relative to the repo root)
// so the Go //go:embed directive in internal/web/embed.go can pick it up. The
// frontend/ source tree builds into ../dist; the dev server proxies /api,
// /login, /logout, and /healthz to the Go daemon on localhost:8080.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: resolve(here, '../dist'),
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 1000,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      // POST /login submits credentials to the daemon; GET /login is a
      // top-level browser navigation and should be handled by Vite's
      // SPA fallback so React Router can render the LoginPage. The
      // daemon-side GET /login redirects authenticated users to / —
      // a behaviour mirrored client-side by LoginPage's auth-status
      // probe.
      '/login': {
        target: 'http://localhost:8080',
        bypass: (req) => {
          if (req.method === 'GET') {
            return req.url
          }
        },
      },
      '/logout': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
})
