export const MAX_COMPLETED_REQUEST_LOGS = 105

type RequestLogWithID = {
  id: number
}

export const mergeCompletedRequestLogs = <T extends RequestLogWithID>(
  current: readonly T[],
  incoming: readonly T[],
  limit = MAX_COMPLETED_REQUEST_LOGS,
): T[] => {
  const boundedLimit = Math.min(Math.max(Math.trunc(limit), 0), MAX_COMPLETED_REQUEST_LOGS)
  if (boundedLimit === 0) return []

  const byID = new Map<number, T>()
  for (const item of current) {
    byID.set(item.id, item)
  }
  for (const item of incoming) {
    byID.set(item.id, item)
  }

  return Array.from(byID.values())
    .sort((left, right) => right.id - left.id)
    .slice(0, boundedLimit)
}
