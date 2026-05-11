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
    it(`${file} outer wrapper does not constrain width with max-w-* or mx-auto`, () => {
      const source = readFileSync(join(here, file), 'utf8')
      // Locate the first <div className after return(, allowing any whitespace/newlines.
      // Handles className="...", className={'...'}, and className={`...`} forms.
      const outer = source.match(/return\s*\([\s\S]*?<div\s[^>]*className=(?:\{)?["'`]([^"'`]*)["'`](?:\})?/)
      expect(outer, `could not locate outer wrapper className in ${file}`).not.toBeNull()
      const classes = outer![1]
      expect(classes, `${file} outer wrapper must not have any max-w-* class`).not.toMatch(/\bmax-w-\S+/)
      expect(classes, `${file} outer wrapper must not have mx-auto`).not.toMatch(/\bmx-auto\b/)
    })
  }
})
