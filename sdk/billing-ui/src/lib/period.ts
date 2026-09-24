import type { Translator } from "../i18n/messages"

// OpenRails durations are exact hours (720h is 30 days, never "a month"), so
// labels name only units that divide the window exactly.
export type PeriodUnit = "hour" | "day" | "week"

export interface Period {
  unit: PeriodUnit
  count: number
}

export function periodOf(hours: number | null | undefined): Period | null {
  if (!hours || !Number.isSafeInteger(hours) || hours <= 0) return null
  if (hours % 168 === 0) return { unit: "week", count: hours / 168 }
  if (hours % 24 === 0) return { unit: "day", count: hours / 24 }
  return { unit: "hour", count: hours }
}

/** Hours from an OpenRails interval (`"720h"`, `"720h0m0s"`). */
export function intervalHours(interval: string | null | undefined) {
  const match = /^(\d+)h(?:0m(?:0s)?)?$/.exec(interval?.trim() ?? "")
  return match ? Number(match[1]) : null
}

/** "every 30 days" */
export function everyLabel(hours: number | null | undefined, m: Translator) {
  const p = periodOf(hours)
  return p ? m.plural(`interval.every.${p.unit}`, p.count) : null
}

/** "/ 30 days" */
export function perLabel(hours: number | null | undefined, m: Translator) {
  const p = periodOf(hours)
  return p ? m.plural(`interval.per.${p.unit}`, p.count) : null
}

/** "30 days of access", for a window that does not renew. */
export function accessLabel(hours: number | null | undefined, m: Translator) {
  const p = periodOf(hours)
  return p ? m.plural(`interval.access.${p.unit}`, p.count) : null
}
