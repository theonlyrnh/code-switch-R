import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const settingsSource = readFileSync(new URL('../src/components/General/Index.vue', import.meta.url), 'utf8')

test('settings no longer exposes configurable network listeners', () => {
  assert.doesNotMatch(settingsSource, /NetworkSettings/)
  assert.doesNotMatch(settingsSource, /SaveNetworkSettings/)
})
