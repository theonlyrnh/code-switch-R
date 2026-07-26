import { Call } from '@wailsio/runtime'

export type LogPlatform = 'claude' | 'openai-responses' | 'openai-chat'

export type RequestLog = {
  id: number
  platform: LogPlatform | ''
  model: string
  provider: string
  relay_key_id?: string
  relay_key_name?: string
  http_code: number
  input_tokens: number
  output_tokens: number
  cache_create_tokens: number
  cache_read_tokens: number
  reasoning_tokens: number
  is_stream?: boolean | number
  duration_sec?: number
  first_token_duration_sec?: number
  client_ip?: string
  upstream_header_sec?: number
  first_event_sec?: number
  first_text_sec?: number
  error_message?: string
  created_at: string
  status?: 'queued' | 'processing' | 'retrying' | 'completed' | string
  retry_requested?: boolean
  queue_position?: number
  queue_started_at?: string
}

export const fetchActiveRequestLogs = async (): Promise<RequestLog[]> => {
  return Call.ByName('codeswitch/services.LogService.ListActiveRequestLogs')
}

export const fetchCompletedRequestLogs = async (
  afterID: number,
  limit: number,
): Promise<RequestLog[]> => {
  return Call.ByName('codeswitch/services.LogService.ListCompletedRequestLogs', afterID, limit)
}

export type RetryActiveRequestResult = {
  status:
    | 'retried'
    | 'ignored_finished'
    | 'ignored_first_text'
    | 'ignored_response_started'
    | 'ignored_unauthorized'
    | 'ignored_queued'
    | 'ignored_transition'
    | string
  first_token_duration_sec?: number
  first_text_sec?: number
}

export const retryActiveRequest = async (id: number): Promise<RetryActiveRequestResult> => {
  return Call.ByName('codeswitch/services.LogService.RetryActiveRequest', id)
}

export type LogStatsSeries = {
  day: string
  total_requests: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cache_create_tokens: number
  cache_read_tokens: number
}

export type LogStats = {
  total_requests: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cache_create_tokens: number
  cache_read_tokens: number
  series: LogStatsSeries[]
}

export const fetchLogStats = async (platform: LogPlatform | '' = ''): Promise<LogStats> => {
  return Call.ByName('codeswitch/services.LogService.StatsSince', platform)
}

export type ProviderDailyStat = {
  provider: string
  total_requests: number
  successful_requests: number
  failed_requests: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cache_create_tokens: number
  cache_read_tokens: number
}

export const fetchProviderDailyStats = async (
  platform: LogPlatform | '' = '',
): Promise<ProviderDailyStat[]> => {
  return Call.ByName('codeswitch/services.LogService.ProviderDailyStats', platform)
}
