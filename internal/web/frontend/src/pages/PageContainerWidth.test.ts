import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { describe, expect, it } from 'vitest'

const here = dirname(fileURLToPath(import.meta.url))

// Pages that must render at full viewport width — no centered max-width column
// on the outer wrapper. LoginPage is intentionally narrow and not listed.
// ForgePage keeps inner max-w-* classes on chat bubbles; only the outer
// wrapper must not constrain width.
const fullWidthPages = [
  'DashboardPage.tsx',
  'CostsPage.tsx',
  'HistoryPage.tsx',
  'CruciblesPage.tsx',
  'PRsPage.tsx',
  'IngotsPage.tsx',
  'BeadDetailPage.tsx',
  'ForgePage.tsx',
]

describe('top-level page wrappers', () => {
  for (const file of fullWidthPages) {
    it(`${file} outer wrapper does not constrain width with max-w-7xl`, () => {
      const source = readFileSync(join(here, file), 'utf8')
      const outer = source.match(/return \(\s*\n\s*<div className="([^"]+)"/)
      expect(outer, `could not locate outer wrapper className in ${file}`).not.toBeNull()
      const classes = outer![1]
      expect(classes).not.toMatch(/\bmax-w-7xl\b/)
      expect(classes).not.toMatch(/\bmx-auto\b/)
    })
  }
})
