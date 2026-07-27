import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const readSource = (relativePath: string) => readFile(new URL(relativePath, import.meta.url), 'utf8')

test('account pool provider keys are collapsed by default and remain user-toggleable', async () => {
  const source = await readSource('../src/components/Main/PoolPanel.vue')

  assert.match(source, /const expandedAccountPoolKeys = ref<Set<string>>\(new Set\(\)\)/)
  assert.match(
    source,
    /const isAccountKeysCollapsed = \(poolID: string\): boolean => !expandedAccountPoolKeys\.value\.has\(poolID\)/,
  )

  const toggleStart = source.indexOf('const toggleAccountKeysCollapsed = (poolID: string) =>')
  const toggleEnd = source.indexOf('\n}', toggleStart) + 2
  const toggleHandler = source.slice(toggleStart, toggleEnd)
  assert.match(toggleHandler, /new Set\(expandedAccountPoolKeys\.value\)/)
  assert.match(toggleHandler, /next\.delete\(poolID\)/)
  assert.match(toggleHandler, /next\.add\(poolID\)/)
  assert.match(toggleHandler, /expandedAccountPoolKeys\.value = next/)
})
