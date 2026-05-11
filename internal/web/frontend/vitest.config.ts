/// <reference types="vitest" />
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// Vitest config is kept separate from vite.config.ts so the prod build does
// not pull in jsdom / testing-library type globals. The Tailwind plugin is
// deliberately not loaded here — tests assert on classes/markup, not styles.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
