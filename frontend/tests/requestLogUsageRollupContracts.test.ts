import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const readSource = (relativePath: string) => readFile(new URL(relativePath, import.meta.url), 'utf8')

const topLevelBlock = (source: string, signature: string, nextSignature: string) => {
  const start = source.indexOf(signature)
  assert.notEqual(start, -1, `missing source block: ${signature}`)
  const end = source.indexOf(nextSignature, start + signature.length)
  assert.notEqual(end, -1, `missing source block terminator: ${nextSignature}`)
  return source.slice(start, end)
}

test('Logs restores usage summary and half-hour chart without provider queries', async () => {
  const pageSource = await readSource('../src/components/Logs/Index.vue')
  const serviceSource = await readSource('../src/services/logs.ts')

  assert.match(pageSource, /<section class="logs-summary"/)
  assert.match(pageSource, /v-for="card in statsCards"/)
  assert.match(pageSource, /<section class="logs-chart"/)
  assert.match(pageSource, /<Line :data="chartData" :options="chartOptions"/)
  assert.match(pageSource, /const statsSeries = computed<LogStatsSeries\[\]>/)
  assert.match(pageSource, /const chartData = computed\(\(\) =>/)
  assert.match(pageSource, /const statsCards = computed\(\(\) =>/)
  assert.match(pageSource, /return `\$\{bucketTime\[1\]\}:\$\{bucketTime\[2\]\}`/)

  assert.doesNotMatch(pageSource, /fetchLogProviders|ListProviders/)
  assert.doesNotMatch(serviceSource, /ListProviders/)
})

test('Logs refreshes rollup stats only on initial load, new completions, and manual refresh', async () => {
  const source = await readSource('../src/components/Logs/Index.vue')

  const loadStats = topLevelBlock(source, 'const loadStats = (): Promise<void> =>', '\nconst loadInitialLogs =')
  assert.match(loadStats, /fetchLogStats\(''\)/)
  assert.equal((source.match(/fetchLogStats\(/g) ?? []).length, 1)

  const initialLoad = topLevelBlock(source, 'const loadInitialLogs = async () =>', '\nconst refreshLogSnapshot =')
  assert.match(initialLoad, /loadStats\(\)/)

  const refreshSnapshot = topLevelBlock(source, 'const refreshLogSnapshot = async () =>', '\nconst refreshLogs =')
  assert.match(refreshSnapshot, /return incomingCompleted\.length > 0/)
  assert.doesNotMatch(refreshSnapshot, /loadStats\(/)

  const refreshLogs = topLevelBlock(source, 'const refreshLogs = (): Promise<boolean> =>', '\nconst pagedLogs =')
  assert.match(refreshLogs, /const snapshotFoundNew = await refreshLogSnapshot\(\)/)
  assert.match(refreshLogs, /statsNeedRefresh = snapshotFoundNew \|\| statsNeedRefresh/)
  assert.match(refreshLogs, /if \(!refreshPending && statsNeedRefresh\)/)
  assert.match(refreshLogs, /await loadStats\(\)/)

  const manualRefresh = topLevelBlock(source, 'const manualRefresh = () =>', '\nconst nextPage =')
  assert.match(manualRefresh, /const (?:foundNewCompleted|statsRefreshed) = await refreshLogs\(\)/)
  assert.match(manualRefresh, /if \(!(?:foundNewCompleted|statsRefreshed)\)/)
  assert.match(manualRefresh, /await loadStats\(\)/)

  const timer = topLevelBlock(source, 'const startLogAutoRefresh = () =>', '\nconst stopLogAutoRefresh =')
  assert.match(timer, /setInterval\(/)
  assert.match(timer, /refreshLogs\(\)/)
  assert.doesNotMatch(timer, /loadStats\(|fetchLogStats\(/)
  assert.doesNotMatch(source, /stats(?:Refresh|Polling|AutoRefresh)Timer/i)
})

test('Settings reads today traffic counters by point lookup and refreshes them every 30 seconds', async () => {
  const logsPageSource = await readSource('../src/components/Logs/Index.vue')
  const settingsPageSource = await readSource('../src/components/General/Index.vue')
  const serviceSource = await readSource('../src/services/logs.ts')

  assert.match(serviceSource, /TrafficService\.Today/)
  assert.doesNotMatch(serviceSource, /TrafficService\.SummarySince/)
  assert.match(serviceSource, /relay_client: TrafficBreakdown/)
  assert.match(serviceSource, /ingress_bytes: number/)
  assert.match(serviceSource, /egress_bytes: number/)
  assert.doesNotMatch(serviceSource, /network_interface|relay_client_public|relay_client_local/)
  assert.doesNotMatch(logsPageSource, /fetchTrafficSummary|trafficCards|traffic-band/)
  assert.match(settingsPageSource, /TRAFFIC_AUTO_REFRESH_INTERVAL_MS = 30_000/)
  assert.match(settingsPageSource, /trafficBodyTotal\(data\.relay_client\)/)
  assert.match(settingsPageSource, /trafficBodyTotal\(data\.upstream\)/)
  assert.match(settingsPageSource, /trafficBodyTotal\(data\.retry\)/)
  assert.match(settingsPageSource, /trafficBodyTotal\(data\.admin\)/)
  assert.doesNotMatch(settingsPageSource, /interface-public|key: 'local'/)

  const timer = topLevelBlock(
    settingsPageSource,
    'const startTrafficAutoRefresh = () =>',
    '\nconst syncTrafficPollingState =',
  )
  assert.match(timer, /setInterval\(/)
  assert.match(timer, /loadTrafficStats\(\)/)
  assert.doesNotMatch(timer, /fetchLogStats\(/)
})
