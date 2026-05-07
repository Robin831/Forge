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
      '/login': 'http://localhost:8080',
      '/logout': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
})
