import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const runtimeSource = readFileSync(new URL('../src/wails-runtime.ts', import.meta.url), 'utf8')

test('Browser.OpenURL opens external links in a new tab without navigating the current page', () => {
  const openUrlImplementation = runtimeSource.match(
    /export namespace Browser \{[\s\S]*?export function OpenURL[\s\S]*?\n  \}\n\}/,
  )?.[0]

  assert.ok(openUrlImplementation, 'Browser.OpenURL implementation should exist')
  assert.match(openUrlImplementation, /window\.open\(url, '_blank', 'noopener,noreferrer'\)/)
  assert.doesNotMatch(openUrlImplementation, /window\.location/)
})
