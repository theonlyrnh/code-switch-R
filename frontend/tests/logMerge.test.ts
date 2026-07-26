import assert from 'node:assert/strict'
import test from 'node:test'

import {
  MAX_COMPLETED_REQUEST_LOGS,
  mergeCompletedRequestLogs,
} from '../src/services/logMerge.ts'

type TestLog = {
  id: number
  value: string
}

test('completed log increments merge without duplicates and stay newest-first', () => {
  const current: TestLog[] = [
    { id: 5, value: 'old-five' },
    { id: 4, value: 'four' },
    { id: 3, value: 'three' },
  ]
  const incoming: TestLog[] = [
    { id: 7, value: 'seven' },
    { id: 6, value: 'six' },
    { id: 5, value: 'new-five' },
  ]

  const merged = mergeCompletedRequestLogs(current, incoming)

  assert.deepEqual(merged.map((item) => item.id), [7, 6, 5, 4, 3])
  assert.equal(merged.find((item) => item.id === 5)?.value, 'new-five')
})

test('completed log merge never retains more than 105 rows', () => {
  const current = Array.from({ length: 105 }, (_, index) => ({
    id: 105 - index,
    value: `old-${index}`,
  }))
  const incoming = Array.from({ length: 20 }, (_, index) => ({
    id: 125 - index,
    value: `new-${index}`,
  }))

  const merged = mergeCompletedRequestLogs(current, incoming, 10_000)

  assert.equal(merged.length, MAX_COMPLETED_REQUEST_LOGS)
  assert.equal(merged[0]?.id, 125)
  assert.equal(merged.at(-1)?.id, 21)
})
