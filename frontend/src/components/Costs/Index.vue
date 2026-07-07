<template>
  <div class="costs-page">
    <div class="costs-header">
      <div>
        <p class="costs-eyebrow">{{ t('components.costs.eyebrow') }}</p>
        <h1>{{ t('components.costs.title') }}</h1>
        <p class="costs-description">{{ t('components.costs.description') }}</p>
      </div>
      <div class="costs-actions">
        <BaseButton variant="outline" type="button" @click="backToHome">
          {{ t('components.logs.back') }}
        </BaseButton>
        <div class="refresh-indicator">
          <span>{{ t('components.logs.nextRefresh', { seconds: countdown }) }}</span>
          <BaseButton type="button" :disabled="loading" @click="manualRefresh">
            {{ t('components.costs.refresh') }}
          </BaseButton>
        </div>
      </div>
    </div>

    <form class="costs-filter-row" @submit.prevent="applyFilters">
      <label class="filter-field">
        <span>{{ t('components.logs.filters.platform') }}</span>
        <select v-model="filters.platform" class="mac-select">
          <option value="">{{ t('components.logs.filters.allPlatforms') }}</option>
          <option value="claude">Claude</option>
          <option value="openai-responses">OpenAI Responses</option>
          <option value="openai-chat">OpenAI Chat</option>
        </select>
      </label>
      <label class="filter-field">
        <span>{{ t('components.logs.filters.provider') }}</span>
        <select v-model="filters.provider" class="mac-select">
          <option value="">{{ t('components.logs.filters.allProviders') }}</option>
          <option v-for="provider in availableProviderOptions" :key="provider" :value="provider">
            {{ provider }}
          </option>
        </select>
      </label>
      <BaseButton type="submit" :disabled="loading">
        {{ t('components.logs.query') }}
      </BaseButton>
    </form>

    <section class="costs-summary-grid">
      <article class="cost-summary-card highlight">
        <span>{{ t('components.costs.summary.totalCost') }}</span>
        <strong>{{ formatTotalCost(summary.totalCost) }}</strong>
        <small>{{ t('components.costs.summary.todayOnly') }}</small>
      </article>
      <article class="cost-summary-card">
        <span>{{ t('components.costs.summary.billableTokens') }}</span>
        <strong>{{ formatInteger(summary.billableTokens) }}</strong>
        <small>{{ t('components.costs.summary.billableHint') }}</small>
      </article>
      <article class="cost-summary-card">
        <span>{{ t('components.costs.summary.unpricedModels') }}</span>
        <strong>{{ summary.unpricedModels }}</strong>
        <small>{{ t('components.costs.summary.unpricedHint') }}</small>
      </article>
      <article class="cost-summary-card">
        <span>{{ t('components.costs.summary.requests') }}</span>
        <strong>{{ formatInteger(summary.requests) }}</strong>
        <small>{{ t('components.costs.summary.successOnly') }}</small>
      </article>
    </section>

    <section class="costs-panel settings-panel">
      <div class="panel-header">
        <div>
          <h2>{{ t('components.costs.settings.title') }}</h2>
          <p>{{ t('components.costs.settings.unit') }}</p>
        </div>
        <BaseButton type="button" :disabled="!canEditSettings || saving" @click="saveSettings">
          {{ t('components.costs.settings.save') }}
        </BaseButton>
      </div>

      <div v-if="!canEditSettings" class="settings-empty">
        {{ t('components.costs.settings.chooseProvider') }}
      </div>
      <div v-else class="settings-content">
        <label class="multiplier-field">
          <span>{{ t('components.costs.settings.multiplier') }}</span>
          <BaseInput v-model="multiplierInput" type="number" min="0.000001" step="0.000001" />
          <span class="multiplier-suffix">x</span>
          <BaseButton variant="outline" type="button" :disabled="saving" @click="resetMultiplier">
            {{ t('components.costs.settings.resetMultiplier') }}
          </BaseButton>
        </label>

        <div class="price-editor-list">
          <article v-for="row in editableRows" :key="row.key" class="price-editor-card">
            <div class="price-editor-title">
              <strong>{{ row.model }}</strong>
              <span v-if="row.defaultPrice" class="source-pill">{{ t('components.costs.settings.defaultPrice') }}</span>
              <span v-else class="missing-pill">{{ t('components.costs.missingPrice') }}</span>
            </div>
            <div class="price-editor-fields">
              <label>
                <span>{{ t('components.costs.price.input') }}</span>
                <BaseInput v-model="priceDrafts[row.modelKey].input" type="number" min="0" step="0.000001" :placeholder="formatPricePlaceholder(row.defaultPrice?.input)" />
              </label>
              <label>
                <span>{{ t('components.costs.price.output') }}</span>
                <BaseInput v-model="priceDrafts[row.modelKey].output" type="number" min="0" step="0.000001" :placeholder="formatPricePlaceholder(row.defaultPrice?.output)" />
              </label>
              <label>
                <span>{{ t('components.costs.price.cacheRead') }}</span>
                <BaseInput v-model="priceDrafts[row.modelKey].cache_read" type="number" min="0" step="0.000001" :placeholder="formatPricePlaceholder(row.defaultPrice?.cache_read)" />
              </label>
              <BaseButton variant="outline" type="button" :disabled="saving" @click="resetOverride(row.model)">
                {{ t('components.costs.settings.clearOverride') }}
              </BaseButton>
            </div>
          </article>
        </div>
      </div>
    </section>

    <section class="costs-panel">
      <div class="panel-header">
        <div>
          <h2>{{ t('components.costs.details.title') }}</h2>
          <p>{{ detailHint }}</p>
        </div>
      </div>

      <div v-if="loading" class="empty-state">{{ t('components.logs.loading') }}</div>
      <div v-else-if="!groups.length" class="empty-state">{{ t('components.costs.empty') }}</div>
      <div v-else class="cost-detail-groups">
        <article v-for="group in groups" :key="group.key" class="provider-group">
          <h3 v-if="showProviderGroups" class="provider-group-title">
            <span class="provider-group-platform">{{ group.platform || '—' }}</span>
            <span>{{ group.provider || t('components.costs.unknownProvider') }}</span>
          </h3>
          <div class="model-table-wrapper">
            <table class="model-cost-table">
              <thead>
                <tr>
                  <th>{{ t('components.costs.table.model') }}</th>
                  <th>{{ t('components.costs.table.requests') }}</th>
                  <th>{{ t('components.costs.table.tokens') }}</th>
                  <th>{{ t('components.costs.table.prices') }}</th>
                  <th>{{ t('components.costs.table.multiplier') }}</th>
                  <th>{{ t('components.costs.table.subtotal') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in group.rows" :key="row.key" :class="{ 'is-unpriced': !row.price }">
                  <td class="model-name-cell">
                    <strong>{{ row.model || '—' }}</strong>
                    <span v-if="!row.price" class="missing-pill">{{ t('components.costs.missingPrice') }}</span>
                    <span v-else-if="row.priceSource === 'override'" class="override-pill">{{ t('components.costs.overridePrice') }}</span>
                    <span v-else class="source-pill">{{ row.defaultPrice?.source }}</span>
                  </td>
                  <td>{{ formatInteger(row.totalRequests) }}</td>
                  <td class="tokens-cell">
                    <span>{{ t('components.costs.tokenLabels.input') }}: {{ formatInteger(row.inputTokens) }}</span>
                    <span>{{ t('components.costs.tokenLabels.output') }}: {{ formatInteger(row.outputTokens) }}</span>
                    <span>{{ t('components.costs.tokenLabels.cacheRead') }}: {{ formatInteger(row.cacheReadTokens) }}</span>
                    <span class="not-billed">{{ t('components.costs.tokenLabels.cacheCreate') }}: {{ formatInteger(row.cacheCreateTokens) }}</span>
                    <span class="not-billed">{{ t('components.costs.tokenLabels.reasoning') }}: {{ formatInteger(row.reasoningTokens) }}</span>
                  </td>
                  <td class="prices-cell">
                    <span>{{ t('components.costs.price.input') }}: {{ formatPrice(row.price?.input) }}</span>
                    <span>{{ t('components.costs.price.output') }}: {{ formatPrice(row.price?.output) }}</span>
                    <span>{{ t('components.costs.price.cacheRead') }}: {{ formatPrice(row.price?.cache_read) }}</span>
                  </td>
                  <td>{{ formatMultiplier(row.multiplier) }}</td>
                  <td class="subtotal-cell">
                    <strong v-if="row.price">{{ formatSubtotal(row.subtotal) }}</strong>
                    <span v-else>{{ t('components.costs.notCounted') }}</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </article>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import BaseButton from '../common/BaseButton.vue'
import BaseInput from '../common/BaseInput.vue'
import { LoadProviders } from '../../../bindings/codeswitch/services/providerservice'
import { fetchLogProviders, type LogPlatform } from '../../services/logs'
import {
  costModelKey,
  costProviderKey,
  defaultCostSettings,
  fetchCostSettings,
  fetchTodayCostUsage,
  findDefaultPrice,
  normalizeCostModelName,
  resetModelOverride,
  resetProviderMultiplier,
  saveCostSettings,
  type CostPrice,
  type CostSettings,
  type CostUsageItem,
  type DefaultModelPrice,
} from '../../services/costs'
import { showToast } from '../../utils/toast'

const { t } = useI18n()
const router = useRouter()

const loading = ref(false)
const saving = ref(false)
const usageItems = ref<CostUsageItem[]>([])
const settings = ref<CostSettings>(defaultCostSettings())
const providerOptions = ref<string[]>([])
const COST_PROVIDER_PLATFORMS: LogPlatform[] = ['claude', 'openai-responses', 'openai-chat']
const multiplierInput = ref('1')
const priceDrafts = reactive<Record<string, { input: string; output: string; cache_read: string }>>({})
const filters = reactive<{ platform: LogPlatform | ''; provider: string }>({ platform: '', provider: '' })
const REFRESH_INTERVAL = 30
const countdown = ref(REFRESH_INTERVAL)
let timer: number | undefined
let initialLoadDone = false

type DraftSyncMode = 'reset' | 'preserve' | false
type LoadCostsOptions = {
  draftSync?: DraftSyncMode
  silent?: boolean
}

const canEditSettings = computed(() => Boolean(filters.platform && filters.provider))
const showProviderGroups = computed(() => filters.provider === '')
const availableProviderOptions = computed(() => {
  const options = new Set<string>()
  for (const provider of providerOptions.value) {
    if (provider.trim()) {
      options.add(provider)
    }
  }
  for (const item of usageItems.value) {
    if ((!filters.platform || item.platform === filters.platform) && item.provider.trim()) {
      options.add(item.provider)
    }
  }
  return Array.from(options).sort((a, b) => a.localeCompare(b))
})

const backToHome = () => router.push('/')

const currentProviderKey = computed(() => canEditSettings.value ? costProviderKey(filters.platform, filters.provider) : '')

const multiplier = (platform: string, provider: string) => {
  const value = settings.value.provider_multipliers[costProviderKey(platform, provider)]
  return value && value > 0 ? value : 1
}

const overridePriceFor = (platform: string, provider: string, model: string): CostPrice | null => {
  return settings.value.model_price_overrides[costModelKey(platform, provider, model)] ?? null
}

const effectivePriceFor = (item: CostUsageItem): { price: CostPrice | null; source: 'override' | 'default' | 'missing'; defaultPrice: DefaultModelPrice | null } => {
  const override = overridePriceFor(item.platform, item.provider, item.model)
  const defaultPrice = findDefaultPrice(item.platform, item.model)
  if (override) return { price: override, source: 'override', defaultPrice }
  if (defaultPrice) return { price: defaultPrice, source: 'default', defaultPrice }
  return { price: null, source: 'missing', defaultPrice: null }
}

const billableInputTokens = (item: CostUsageItem) => Math.max(item.input_tokens - item.cache_read_tokens, 0)
const billableTokenTotal = (item: CostUsageItem) => billableInputTokens(item) + item.output_tokens + item.cache_read_tokens

const rowViewModels = computed(() => usageItems.value.map((item) => {
  const priceInfo = effectivePriceFor(item)
  const rowMultiplier = multiplier(item.platform, item.provider)
  const chargedInputTokens = billableInputTokens(item)
  const subtotal = priceInfo.price
    ? ((chargedInputTokens * priceInfo.price.input)
      + (item.output_tokens * priceInfo.price.output)
      + (item.cache_read_tokens * priceInfo.price.cache_read)) / 1_000_000 * rowMultiplier
    : 0
  return {
    key: `${item.platform}::${item.provider}::${item.model}`,
    platform: item.platform,
    provider: item.provider,
    model: item.model,
    modelKey: normalizeCostModelName(item.model),
    totalRequests: item.total_requests,
    inputTokens: item.input_tokens,
    outputTokens: item.output_tokens,
    cacheCreateTokens: item.cache_create_tokens,
    cacheReadTokens: item.cache_read_tokens,
    reasoningTokens: item.reasoning_tokens,
    billableTokens: billableTokenTotal(item),
    multiplier: rowMultiplier,
    subtotal,
    price: priceInfo.price,
    priceSource: priceInfo.source,
    defaultPrice: priceInfo.defaultPrice,
  }
}))

const editableRows = computed(() => rowViewModels.value.filter((row) => row.platform === filters.platform && row.provider === filters.provider))

const groups = computed(() => {
  const map = new Map<string, { key: string; platform: string; provider: string; rows: typeof rowViewModels.value }>()
  for (const row of rowViewModels.value) {
    const key = showProviderGroups.value ? `${row.platform}::${row.provider}` : 'selected'
    if (!map.has(key)) {
      map.set(key, { key, platform: row.platform, provider: row.provider, rows: [] })
    }
    map.get(key)?.rows.push(row)
  }
  return Array.from(map.values())
})

const summary = computed(() => {
  const priced = rowViewModels.value.filter((row) => row.price)
  const unpricedModelKeys = new Set(rowViewModels.value.filter((row) => !row.price).map((row) => row.key))
  return {
    totalCost: priced.reduce((sum, row) => sum + row.subtotal, 0),
    billableTokens: rowViewModels.value.reduce((sum, row) => sum + row.billableTokens, 0),
    unpricedModels: unpricedModelKeys.size,
    requests: rowViewModels.value.reduce((sum, row) => sum + row.totalRequests, 0),
  }
})

const detailHint = computed(() => showProviderGroups.value
  ? t('components.costs.details.groupedByProvider')
  : t('components.costs.details.groupedByModel'))

const formatInteger = (value?: number) => new Intl.NumberFormat().format(value ?? 0)
const formatTotalCost = (value: number) => `$${value < 0.01 ? value.toFixed(6) : value.toFixed(4)}`
const formatSubtotal = (value: number) => `$${value.toFixed(6)}`
const formatMultiplier = (value: number) => `${trimNumber(value)}x`
const formatPrice = (value?: number) => value == null ? '—' : `$${trimNumber(value)}`
const formatPricePlaceholder = (value?: number) => value == null ? '' : trimNumber(value)
const trimNumber = (value: number) => Number(value.toFixed(6)).toString()

const normalizeProviderOption = (value: string | undefined) => (value ?? '').trim()

const loadConfiguredProviderNames = async (platform: LogPlatform | '') => {
  const platforms = platform ? [platform] : COST_PROVIDER_PLATFORMS
  const results = await Promise.allSettled(platforms.map((kind) => LoadProviders(kind)))
  const names: string[] = []
  for (const result of results) {
    if (result.status !== 'fulfilled') {
      console.error('failed to load configured providers', result.reason)
      continue
    }
    for (const provider of result.value ?? []) {
      const name = normalizeProviderOption(provider.name)
      if (name) {
        names.push(name)
      }
    }
  }
  return names
}

const loadLogProviderNames = async (platform: LogPlatform | '') => {
  try {
    const providers = await fetchLogProviders(platform)
    return (providers ?? []).map(normalizeProviderOption).filter(Boolean)
  } catch (error) {
    console.error('failed to load log providers', error)
    return []
  }
}

const loadProviders = async () => {
  const [configuredProviders, logProviders] = await Promise.all([
    loadConfiguredProviderNames(filters.platform),
    loadLogProviderNames(filters.platform),
  ])
  const merged = new Set<string>()
  for (const provider of [...configuredProviders, ...logProviders]) {
    const name = normalizeProviderOption(provider)
    if (name) {
      merged.add(name)
    }
  }
  providerOptions.value = Array.from(merged).sort((a, b) => a.localeCompare(b))
  if (filters.provider && providerOptions.value.length > 0 && !providerOptions.value.includes(filters.provider)) {
    filters.provider = ''
  }
}

const loadDashboard = async (options: LoadCostsOptions = {}) => {
  await loadProviders()
  await loadData(options)
}

const loadData = async (options: LoadCostsOptions = {}) => {
  const draftSync = options.draftSync ?? 'preserve'
  if (!options.silent) {
    loading.value = true
  }
  try {
    const [nextSettings, nextUsage] = await Promise.all([
      fetchCostSettings(),
      fetchTodayCostUsage(filters.platform, filters.provider),
    ])
    settings.value = nextSettings
    usageItems.value = nextUsage
    if (draftSync === 'reset') {
      syncDrafts()
    } else if (draftSync === 'preserve') {
      syncDrafts({ preserveExisting: true })
    }
  } catch (error: any) {
    showToast(error?.message || t('components.costs.loadFailed'), 'error')
  } finally {
    if (!options.silent) {
      loading.value = false
    }
  }
}

const resetTimer = () => {
  countdown.value = REFRESH_INTERVAL
}

const startCountdown = () => {
  stopCountdown()
  timer = window.setInterval(() => {
    if (countdown.value <= 1) {
      countdown.value = REFRESH_INTERVAL
      void loadDashboard({ draftSync: 'preserve', silent: true })
    } else {
      countdown.value -= 1
    }
  }, 1000)
}

const stopCountdown = () => {
  if (timer) {
    clearInterval(timer)
    timer = undefined
  }
}

const manualRefresh = () => {
  resetTimer()
  void loadDashboard({ draftSync: 'preserve', silent: true })
}

const applyFilters = () => {
  resetTimer()
  void loadDashboard({ draftSync: 'preserve' })
}

const syncDrafts = (options: { preserveExisting?: boolean } = {}) => {
  const preserveExisting = options.preserveExisting ?? false
  if (!currentProviderKey.value) {
    if (!preserveExisting) {
      multiplierInput.value = '1'
    }
    return
  }

  if (!preserveExisting || multiplierInput.value.trim() === '') {
    multiplierInput.value = String(settings.value.provider_multipliers[currentProviderKey.value] ?? 1)
  }

  for (const row of editableRows.value) {
    const key = row.modelKey
    if (preserveExisting && priceDrafts[key]) {
      continue
    }
    const override = overridePriceFor(row.platform, row.provider, row.model)
    priceDrafts[key] = {
      input: override ? String(override.input) : '',
      output: override ? String(override.output) : '',
      cache_read: override ? String(override.cache_read) : '',
    }
  }
}

const parseOptionalPrice = (raw: string): number | null => {
  const trimmed = raw.trim()
  if (trimmed === '') return null
  const value = Number(trimmed)
  if (!Number.isFinite(value) || value < 0) return null
  return value
}

const saveSettings = async () => {
  if (!canEditSettings.value) return
  saving.value = true
  try {
    const nextSettings: CostSettings = {
      provider_multipliers: { ...settings.value.provider_multipliers },
      model_price_overrides: { ...settings.value.model_price_overrides },
    }
    const parsedMultiplier = Number(multiplierInput.value)
    if (Number.isFinite(parsedMultiplier) && parsedMultiplier > 0 && parsedMultiplier !== 1) {
      nextSettings.provider_multipliers[currentProviderKey.value] = parsedMultiplier
    } else {
      delete nextSettings.provider_multipliers[currentProviderKey.value]
    }

    for (const row of editableRows.value) {
      const draft = priceDrafts[row.modelKey]
      if (!draft) continue
      const input = parseOptionalPrice(draft.input)
      const output = parseOptionalPrice(draft.output)
      const cacheRead = parseOptionalPrice(draft.cache_read)
      const key = costModelKey(row.platform, row.provider, row.model)
      if (input == null && output == null && cacheRead == null) {
        delete nextSettings.model_price_overrides[key]
      } else {
        nextSettings.model_price_overrides[key] = {
          input: input ?? row.defaultPrice?.input ?? 0,
          output: output ?? row.defaultPrice?.output ?? 0,
          cache_read: cacheRead ?? row.defaultPrice?.cache_read ?? 0,
        }
      }
    }

    settings.value = await saveCostSettings(nextSettings)
    syncDrafts()
    showToast(t('components.costs.settings.saved'), 'success')
  } catch (error: any) {
    showToast(error?.message || t('components.costs.settings.saveFailed'), 'error')
  } finally {
    saving.value = false
  }
}

const resetMultiplier = async () => {
  if (!canEditSettings.value) return
  saving.value = true
  try {
    await resetProviderMultiplier(filters.platform, filters.provider)
    delete settings.value.provider_multipliers[currentProviderKey.value]
    multiplierInput.value = '1'
    showToast(t('components.costs.settings.resetDone'), 'success')
  } catch (error: any) {
    showToast(error?.message || t('components.costs.settings.saveFailed'), 'error')
  } finally {
    saving.value = false
  }
}

const resetOverride = async (model: string) => {
  if (!canEditSettings.value) return
  saving.value = true
  try {
    await resetModelOverride(filters.platform, filters.provider, model)
    delete settings.value.model_price_overrides[costModelKey(filters.platform, filters.provider, model)]
    const key = normalizeCostModelName(model)
    priceDrafts[key] = { input: '', output: '', cache_read: '' }
    showToast(t('components.costs.settings.resetDone'), 'success')
  } catch (error: any) {
    showToast(error?.message || t('components.costs.settings.saveFailed'), 'error')
  } finally {
    saving.value = false
  }
}

watch(() => filters.platform, async () => {
  resetTimer()
  await loadDashboard({ draftSync: 'reset' })
})

watch(() => filters.provider, async () => {
  resetTimer()
  await loadData({ draftSync: 'reset' })
})

onMounted(async () => {
  await loadDashboard({ draftSync: 'reset' })
  initialLoadDone = true
  startCountdown()
})

onActivated(() => {
  if (!initialLoadDone) return
  startCountdown()
  void loadDashboard({ draftSync: 'preserve', silent: true })
})

onDeactivated(() => {
  stopCountdown()
})

onUnmounted(() => {
  stopCountdown()
})
</script>

<style scoped>
.costs-page {
  padding: 32px;
  color: var(--mac-text);
}

.costs-header {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  align-items: flex-start;
  margin-bottom: 24px;
}

.costs-eyebrow {
  margin: 0 0 8px;
  color: var(--mac-text-secondary);
  font-size: 0.78rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}

.costs-header h1 {
  margin: 0;
  font-size: 2rem;
}

.costs-description {
  margin: 8px 0 0;
  color: var(--mac-text-secondary);
}

.costs-actions,
.costs-filter-row {
  display: flex;
  gap: 12px;
  align-items: flex-end;
  flex-wrap: wrap;
}

.costs-filter-row {
  margin-bottom: 20px;
  padding: 16px;
  border: 1px solid var(--mac-border);
  border-radius: 14px;
  background: var(--mac-surface);
}

.filter-field {
  display: grid;
  gap: 6px;
  min-width: 220px;
  color: var(--mac-text-secondary);
  font-size: 0.9rem;
}

.mac-select {
  min-height: 36px;
  border-radius: 8px;
  border: 1px solid var(--mac-border);
  background: var(--mac-bg);
  color: var(--mac-text);
  padding: 0 10px;
}

.costs-summary-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 14px;
  margin-bottom: 20px;
}

.cost-summary-card,
.costs-panel {
  border: 1px solid var(--mac-border);
  background: var(--mac-surface);
  border-radius: 16px;
  box-shadow: var(--mac-shadow-sm);
}

.cost-summary-card {
  padding: 18px;
  display: grid;
  gap: 8px;
}

.cost-summary-card span,
.cost-summary-card small,
.panel-header p,
.settings-empty {
  color: var(--mac-text-secondary);
}

.cost-summary-card strong {
  font-size: 1.55rem;
}

.cost-summary-card.highlight strong {
  color: var(--mac-accent);
}

.costs-panel {
  padding: 18px;
  margin-bottom: 20px;
}

.panel-header {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  align-items: flex-start;
  margin-bottom: 16px;
}

.panel-header h2 {
  margin: 0 0 4px;
}

.panel-header p {
  margin: 0;
}

.settings-empty,
.empty-state {
  padding: 28px;
  text-align: center;
}

.settings-content {
  display: grid;
  gap: 16px;
}

.multiplier-field {
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
}

.multiplier-field input {
  width: 140px;
}

.multiplier-suffix {
  color: var(--mac-text-secondary);
}

.price-editor-list,
.cost-detail-groups {
  display: grid;
  gap: 12px;
}

.price-editor-card {
  border: 1px solid var(--mac-border);
  border-radius: 12px;
  padding: 14px;
  background: var(--mac-bg);
}

.price-editor-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-bottom: 12px;
}

.price-editor-fields {
  display: grid;
  grid-template-columns: repeat(3, minmax(130px, 1fr)) auto;
  gap: 10px;
  align-items: end;
}

.price-editor-fields label {
  display: grid;
  gap: 5px;
  color: var(--mac-text-secondary);
  font-size: 0.85rem;
}

.provider-group-title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin: 0 0 10px;
  border: 1px solid var(--mac-border);
  border-radius: 999px;
  padding: 4px 10px;
  background: var(--mac-bg);
  font-size: 0.9rem;
  font-weight: 600;
  line-height: 1.3;
}

.provider-group-platform {
  color: var(--mac-text-secondary);
  font-size: 0.78rem;
  font-weight: 500;
}

.model-table-wrapper {
  overflow-x: auto;
}

.model-cost-table {
  width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
  min-width: 980px;
}

.model-cost-table th:nth-child(1) {
  width: 22%;
}

.model-cost-table th:nth-child(2) {
  width: 8%;
}

.model-cost-table th:nth-child(3) {
  width: 29%;
}

.model-cost-table th:nth-child(4) {
  width: 18%;
}

.model-cost-table th:nth-child(5) {
  width: 9%;
}

.model-cost-table th:nth-child(6) {
  width: 14%;
}

.model-cost-table th,
.model-cost-table td {
  padding: 12px;
  border-bottom: 1px solid var(--mac-border);
  text-align: left;
  vertical-align: top;
}

.model-cost-table th {
  color: var(--mac-text-secondary);
  font-weight: 600;
  font-size: 0.85rem;
}

.model-name-cell strong,
.model-name-cell span,
.tokens-cell span,
.prices-cell span {
  display: block;
  margin-bottom: 6px;
}

.model-name-cell strong {
  overflow-wrap: anywhere;
}

.tokens-cell,
.prices-cell {
  font-size: 0.88rem;
  line-height: 1.45;
}

.not-billed {
  color: var(--mac-text-secondary);
}

.subtotal-cell strong {
  color: var(--mac-accent);
}

.source-pill,
.override-pill,
.missing-pill {
  display: inline-flex;
  align-items: center;
  width: fit-content;
  border-radius: 999px;
  padding: 2px 8px;
  font-size: 0.75rem;
}

.source-pill {
  background: rgba(34, 197, 94, 0.12);
  color: #16a34a;
}

.override-pill {
  background: rgba(59, 130, 246, 0.12);
  color: #2563eb;
}

.missing-pill {
  background: rgba(245, 158, 11, 0.14);
  color: #d97706;
}

.is-unpriced {
  background: rgba(245, 158, 11, 0.04);
}

@media (max-width: 900px) {
  .costs-page {
    padding: 20px;
  }

  .costs-header,
  .panel-header {
    display: grid;
  }

  .costs-summary-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .price-editor-fields {
    grid-template-columns: 1fr;
  }
}

@media (max-width: 640px) {
  .costs-summary-grid {
    grid-template-columns: 1fr;
  }
}
</style>
