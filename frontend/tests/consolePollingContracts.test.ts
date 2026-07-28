import assert from 'node:assert/strict'
import fs from 'node:fs'
import test from 'node:test'

const source = fs.readFileSync(new URL('../src/components/Console/Index.vue', import.meta.url), 'utf8')

test('console polling requests incremental batches and keeps a bounded window', () => {
  assert.match(source, /ConsoleService\.GetLogUpdates/)
  assert.doesNotMatch(source, /ConsoleService\.GetLogs['"]/)
  assert.match(source, /\.slice\(-MAX_CONSOLE_LOGS\)/)
})
