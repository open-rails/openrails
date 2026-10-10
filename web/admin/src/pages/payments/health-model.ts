// Payments → Health: the metrics queries behind each panel, the shaping of
// their results, and the list URL every tile and cell opens. Metric and list
// filters share names, so a drill-down carries them unchanged; only the PSP
// differs (metrics group by its key, lists take its id).
import type {
  MetricsCell,
  MetricsQuery,
  MetricsResult,
} from "@/lib/api/metrics"
import { indexColumns } from "@/pages/dashboard/lib"

export const OWNERS = [
  { value: "", label: "All owners" },
  { value: "nmi_schedule", label: "NMI-owned" },
  { value: "engine", label: "OpenRails-owned" },
  { value: "provider", label: "Provider-owned" },
] as const

export const RANGES = [
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "90d", label: "Last 90 days" },
] as const

// Scope is the page's controls: owner, PSP id and the trailing range.
export interface Scope {
  owner: string
  psp?: { id: string }
  last: string
}

type Filters = Record<string, string[]>

export const NEW_CARD: Filters = {
  kind: ["verify", "initial"],
  card_entry: ["new"],
}
export const FAILED_CATEGORIES = [
  "card_data",
  "issuer_soft",
  "issuer_hard",
  "gateway_rule",
  "system_error",
  "unknown",
]
export const FIRST_FAILED = ["declined", "error", "missed"]

function scoped(scope: Scope, extra: Filters = {}): Filters | undefined {
  const out: Filters = { ...extra }
  if (scope.owner) out.owner = [scope.owner]
  if (scope.psp) out.psp = [scope.psp.id]
  return Object.keys(out).length ? out : undefined
}

export interface Tile {
  id: string
  title: string
  measure: string
  // lower: a rise is bad news.
  better: "lower" | "higher"
  query: (scope: Scope) => MetricsQuery
  list: "attempts" | "cycles"
  listFilters: Filters
}

const compared = (
  measures: string[],
  scope: Scope,
  filters: Filters = {}
): MetricsQuery => ({
  measures,
  range: { last: scope.last },
  filters: scoped(scope, filters),
  compare: "previous_period",
})

const CYCLE_MEASURES = [
  "rebill_first_failure_rate",
  "rebills_missed",
  "dunning_recovery_rate",
  "rebill_collection_rate",
]
const cycleTiles = (scope: Scope) => compared(CYCLE_MEASURES, scope)

export const TILES: Tile[] = [
  {
    id: "new-card-decline",
    title: "New-card decline rate",
    measure: "attempt_failure_rate",
    better: "lower",
    query: (scope) => compared(["attempt_failure_rate"], scope, NEW_CARD),
    list: "attempts",
    listFilters: { ...NEW_CARD, category: FAILED_CATEGORIES },
  },
  {
    id: "checkout-failure",
    title: "New-card checkout failure rate",
    measure: "checkout_failure_rate",
    better: "lower",
    query: (scope) => compared(["checkout_failure_rate"], scope),
    list: "attempts",
    listFilters: { card_entry: ["new"] },
  },
  {
    id: "rebill-first-failure",
    title: "Rebill first-attempt failure rate",
    measure: "rebill_first_failure_rate",
    better: "lower",
    query: cycleTiles,
    list: "cycles",
    listFilters: { first_outcome: FIRST_FAILED },
  },
  {
    id: "missed",
    title: "Missed rebills",
    measure: "rebills_missed",
    better: "lower",
    query: cycleTiles,
    list: "cycles",
    listFilters: { first_outcome: ["missed"] },
  },
  {
    id: "recovery",
    title: "Dunning recovery rate",
    measure: "dunning_recovery_rate",
    better: "higher",
    query: cycleTiles,
    list: "cycles",
    listFilters: { first_outcome: FIRST_FAILED, outcome: ["collected"] },
  },
  {
    id: "collection",
    title: "Rebill collection rate",
    measure: "rebill_collection_rate",
    better: "higher",
    query: cycleTiles,
    list: "cycles",
    listFilters: { outcome: ["collected", "lost"] },
  },
]

export interface TileValue {
  value: MetricsCell
  unit?: string
  // change is the move from the previous period: percentage points for a
  // rate, the difference for a count; null when either side is empty.
  change: number | null
  worse: boolean
}

export function tileValue(
  result: MetricsResult | undefined,
  tile: Tile
): TileValue | undefined {
  if (!result) return undefined
  const idx = indexColumns(result.columns)
  const m = idx.measures.find((c) => c.name === tile.measure)
  if (!m) return undefined
  const current = result.rows[0]?.[m.index] ?? null
  const previous = result.compare_rows?.[0]?.[m.index] ?? null
  let change: number | null = null
  if (current !== null && previous !== null) {
    change = Number(current) - Number(previous)
    if (!Number.isFinite(change)) change = null
  }
  const worse =
    change !== null &&
    change !== 0 &&
    (tile.better === "lower" ? change > 0 : change < 0)
  return { value: current, unit: m.unit, change, worse }
}

export function formatChange(v: TileValue): string {
  if (v.change === null) return "no previous period"
  const sign = v.change > 0 ? "+" : ""
  if (v.unit === "ratio")
    return `${sign}${(v.change * 100).toFixed(1)} pp vs previous period`
  return `${sign}${v.change.toLocaleString()} vs previous period`
}

export const trendQueries = (scope: Scope) => {
  const weekly = (measure: string, filters: Filters = {}): MetricsQuery => ({
    measures: [measure],
    by: ["time", "owner"],
    grain: "week",
    range: { last: "12w" },
    filters: scoped(scope, filters),
  })
  return {
    newCard: weekly("attempt_failure_rate", NEW_CARD),
    rebill: weekly("rebill_first_failure_rate"),
  }
}

export const reasonsQuery = (scope: Scope): MetricsQuery => ({
  measures: ["failed_attempts"],
  by: ["category", "reason", "response_code"],
  range: { last: scope.last },
  filters: scoped(scope),
  compare: "previous_period",
})

export interface ReasonRow {
  category: string
  reason: string
  code: string
  count: number
  share: number
  // previous is the count in the previous period (0 when absent).
  previous: number
}

export interface ReasonGroup {
  category: string
  count: number
  share: number
  rows: ReasonRow[]
}

// reasonGroups nests category → reason → code, largest first; share is of all
// failed attempts in the range.
export function reasonGroups(result: MetricsResult | undefined): ReasonGroup[] {
  if (!result) return []
  const idx = indexColumns(result.columns)
  const col = (name: string) =>
    idx.dims.find((d) => d.name === name)?.index ?? -1
  const [cat, rsn, code] = [
    col("category"),
    col("reason"),
    col("response_code"),
  ]
  const measure = idx.measures[0]?.index ?? -1
  if (cat < 0 || rsn < 0 || code < 0 || measure < 0) return []
  const key = (r: MetricsCell[]) => JSON.stringify([r[cat], r[rsn], r[code]])
  const previous = new Map<string, number>()
  for (const r of result.compare_rows ?? [])
    previous.set(key(r), Number(r[measure] ?? 0))
  const rows: ReasonRow[] = result.rows.map((r) => ({
    category: String(r[cat] ?? ""),
    reason: String(r[rsn] ?? ""),
    code: String(r[code] ?? ""),
    count: Number(r[measure] ?? 0),
    share: 0,
    previous: previous.get(key(r)) ?? 0,
  }))
  const total = rows.reduce((sum, r) => sum + r.count, 0)
  const groups = new Map<string, ReasonGroup>()
  for (const r of rows) {
    r.share = total ? r.count / total : 0
    let g = groups.get(r.category)
    if (!g) {
      g = { category: r.category, count: 0, share: 0, rows: [] }
      groups.set(r.category, g)
    }
    g.count += r.count
    g.rows.push(r)
  }
  return [...groups.values()]
    .map((g) => ({
      ...g,
      share: total ? g.count / total : 0,
      rows: g.rows.sort((a, b) => b.count - a.count),
    }))
    .sort((a, b) => b.count - a.count)
}

export const checkQueries = (scope: Scope) => {
  const by = (dim: string): MetricsQuery => ({
    measures: ["attempts", "attempt_failure_rate"],
    by: [dim],
    range: { last: scope.last },
    filters: scoped(scope, NEW_CARD),
  })
  return { avs: by("avs_result"), cvv: by("cvv_result") }
}

export interface CheckRow {
  result: string
  attempts: number
  failureRate: number | null
}

export function checkRows(result: MetricsResult | undefined): CheckRow[] {
  if (!result) return []
  const idx = indexColumns(result.columns)
  const dim = idx.dims[0]?.index ?? -1
  const count = idx.measures.find((m) => m.name === "attempts")?.index ?? -1
  const rate =
    idx.measures.find((m) => m.name === "attempt_failure_rate")?.index ?? -1
  if (dim < 0 || count < 0 || rate < 0) return []
  return result.rows
    .map((r) => ({
      result: String(r[dim] ?? ""),
      attempts: Number(r[count] ?? 0),
      failureRate: r[rate] === null ? null : Number(r[rate]),
    }))
    .sort((a, b) => b.attempts - a.attempts)
}

export const RECOVERY_ATTEMPTS = ["1", "2", "3", "4", "5+"]
export const RECOVERY_DAYS = [1, 3, 7, 14, 30]

export const recoveryQueries = (scope: Scope) => {
  const q = (measures: string[], by: string[]): MetricsQuery => ({
    measures,
    by,
    range: { last: scope.last },
    filters: scoped(scope),
  })
  return {
    rate: q(["dunning_recovery_rate", "dunning_recovered"], ["owner"]),
    byAttempt: q(["dunning_recovered"], ["owner", "recovery_attempt"]),
    byDays: q(["dunning_recovered"], ["owner", "days_to_recover"]),
  }
}

export interface RecoveryCurve {
  owner: string
  // points[i] is the share of failed cycles recovered by step i.
  points: number[]
}

// recoveryCurves turns recoveries per step into cumulative shares of failed
// cycles: a step's share is its recoveries over the owner's closed failed
// cycles, which is recovered / recovery rate.
export function recoveryCurves(
  rate: MetricsResult | undefined,
  steps: MetricsResult | undefined,
  dimension: "recovery_attempt" | "days_to_recover"
): RecoveryCurve[] {
  if (!rate || !steps) return []
  const r = indexColumns(rate.columns)
  const s = indexColumns(steps.columns)
  const rOwner = r.dims.find((d) => d.name === "owner")?.index ?? -1
  const rRate =
    r.measures.find((m) => m.name === "dunning_recovery_rate")?.index ?? -1
  const rCount =
    r.measures.find((m) => m.name === "dunning_recovered")?.index ?? -1
  const sOwner = s.dims.find((d) => d.name === "owner")?.index ?? -1
  const sStep = s.dims.find((d) => d.name === dimension)?.index ?? -1
  const sCount = s.measures[0]?.index ?? -1
  if ([rOwner, rRate, rCount, sOwner, sStep, sCount].includes(-1)) return []
  const failed = new Map<string, number>()
  for (const row of rate.rows) {
    const recovered = Number(row[rCount] ?? 0)
    const share = Number(row[rRate] ?? 0)
    if (recovered > 0 && share > 0)
      failed.set(String(row[rOwner]), recovered / share)
  }
  const buckets =
    dimension === "recovery_attempt" ? RECOVERY_ATTEMPTS : RECOVERY_DAYS
  const out: RecoveryCurve[] = []
  for (const [owner, denominator] of failed) {
    const points = buckets.map((bucket) => {
      let recovered = 0
      for (const row of steps.rows) {
        if (String(row[sOwner]) !== owner || row[sStep] === "") continue
        const step = String(row[sStep])
        const within =
          dimension === "recovery_attempt"
            ? RECOVERY_ATTEMPTS.indexOf(step) <=
              RECOVERY_ATTEMPTS.indexOf(String(bucket))
            : Number(step) <= Number(bucket)
        if (within) recovered += Number(row[sCount] ?? 0)
      }
      return recovered / denominator
    })
    out.push({ owner, points })
  }
  return out.sort((a, b) => a.owner.localeCompare(b.owner))
}

export const missedQuery = (scope: Scope): MetricsQuery => ({
  measures: ["rebills_missed"],
  by: ["miss_reason", "psp"],
  range: { last: scope.last },
  filters: scoped(scope),
})

export interface MissedRow {
  reason: string
  psp: string
  count: number
}

export function missedRows(result: MetricsResult | undefined): MissedRow[] {
  if (!result) return []
  const idx = indexColumns(result.columns)
  const reason = idx.dims.find((d) => d.name === "miss_reason")?.index ?? -1
  const psp = idx.dims.find((d) => d.name === "psp")?.index ?? -1
  const count = idx.measures[0]?.index ?? -1
  if (reason < 0 || psp < 0 || count < 0) return []
  return result.rows
    .map((r) => ({
      reason: String(r[reason] ?? ""),
      psp: String(r[psp] ?? ""),
      count: Number(r[count] ?? 0),
    }))
    .filter((r) => r.reason !== "" && r.count > 0)
    .sort((a, b) => b.count - a.count)
}

// coverageQuery: how OpenRails learned of the providers' own charges.
export const coverageQuery = (scope: Scope): MetricsQuery => ({
  measures: ["attempts"],
  by: ["psp", "observed_via"],
  range: { last: scope.last },
  filters: scoped(scope, { source: ["provider_schedule"] }),
})

export interface CoverageRow {
  psp: string
  webhook: number
  pull: number
  // coverage is the share learned by webhook.
  coverage: number | null
}

export function coverageRows(result: MetricsResult | undefined): CoverageRow[] {
  if (!result) return []
  const idx = indexColumns(result.columns)
  const psp = idx.dims.find((d) => d.name === "psp")?.index ?? -1
  const via = idx.dims.find((d) => d.name === "observed_via")?.index ?? -1
  const count = idx.measures[0]?.index ?? -1
  if (psp < 0 || via < 0 || count < 0) return []
  const rows = new Map<string, CoverageRow>()
  for (const r of result.rows) {
    const key = String(r[psp] ?? "")
    let row = rows.get(key)
    if (!row) {
      row = { psp: key, webhook: 0, pull: 0, coverage: null }
      rows.set(key, row)
    }
    const n = Number(r[count] ?? 0)
    if (r[via] === "webhook") row.webhook += n
    else if (r[via] === "pull") row.pull += n
  }
  return [...rows.values()]
    .map((row) => {
      const seen = row.webhook + row.pull
      return { ...row, coverage: seen ? row.webhook / seen : null }
    })
    .sort((a, b) => a.psp.localeCompare(b.psp))
}

// listURL opens the attempt or cycle list with the panel's filters, the page's
// scope and the query's resolved range.
export function listURL(
  list: "attempts" | "cycles",
  filters: Filters,
  scope: Scope,
  range?: { from: string; to: string },
  pspId?: string
): string {
  const p = new URLSearchParams()
  for (const [key, values] of Object.entries(filters))
    if (values.length) p.set(key, values.join(","))
  if (scope.owner && !p.has("owner")) p.set("owner", scope.owner)
  const psp = pspId ?? scope.psp?.id
  if (psp) p.set("psp_id", psp)
  if (range) {
    p.set(list === "attempts" ? "since" : "due_since", range.from)
    p.set(list === "attempts" ? "until" : "due_until", range.to)
  }
  return `/payments/${list}?${p.toString()}`
}

// NMI's own transaction history, read daily per NMI PSP and kept 25 months. It
// has no owner, so only the PSP scopes it.
export const NMI_KINDS = [
  { value: "verification", label: "Card verifications" },
  { value: "one_off_sale", label: "One-off sales" },
  { value: "scheduled_rebill", label: "NMI-scheduled rebills" },
] as const

export const nmiHistoryQueries = (scope: Scope) => {
  const psp: Filters = scope.psp ? { psp: [scope.psp.id] } : {}
  const range = { last: "25m" }
  const months: MetricsQuery = {
    measures: ["nmi_history_authorizations", "nmi_history_refusal_rate"],
    by: ["time", "nmi_kind"],
    grain: "month",
    range,
    filters: scope.psp ? psp : undefined,
  }
  const reasons: MetricsQuery = {
    measures: ["nmi_history_refused"],
    by: ["nmi_kind", "category", "reason"],
    range,
    filters: { ...psp, category: FAILED_CATEGORIES },
  }
  // OpenRails' own NMI attempts by month: the first marks where it began.
  const recorded: MetricsQuery = {
    measures: ["attempts"],
    by: ["time"],
    grain: "month",
    range,
    filters: { ...psp, rail: ["nmi"] },
  }
  return { months, reasons, recorded }
}

// recordingSince is the first month OpenRails recorded an NMI attempt in.
export function recordingSince(
  result: MetricsResult | undefined
): string | null {
  if (!result) return null
  const idx = indexColumns(result.columns)
  const count = idx.measures[0]?.index ?? -1
  if (idx.time < 0 || count < 0) return null
  const months = result.rows
    .filter((r) => Number(r[count] ?? 0) > 0)
    .map((r) => String(r[idx.time]))
    .sort()
  return months[0] ?? null
}

export interface NMIHistoryCell {
  authorizations: number
  refusalRate: number | null
}

export interface NMIHistoryMonth {
  month: string
  kinds: Record<string, NMIHistoryCell>
  // recorded: OpenRails recorded its own attempts by this month.
  recorded: boolean
}

// nmiHistoryMonths is newest first, only months NMI answered anything in.
export function nmiHistoryMonths(
  result: MetricsResult | undefined,
  since: string | null
): NMIHistoryMonth[] {
  if (!result) return []
  const idx = indexColumns(result.columns)
  const kind = idx.dims.find((d) => d.name === "nmi_kind")?.index ?? -1
  const count =
    idx.measures.find((m) => m.name === "nmi_history_authorizations")?.index ??
    -1
  const rate =
    idx.measures.find((m) => m.name === "nmi_history_refusal_rate")?.index ??
    -1
  if ([idx.time, kind, count, rate].includes(-1)) return []
  const months = new Map<string, NMIHistoryMonth>()
  for (const r of result.rows) {
    const authorizations = Number(r[count] ?? 0)
    if (authorizations === 0) continue
    const month = String(r[idx.time])
    let m = months.get(month)
    if (!m) {
      m = { month, kinds: {}, recorded: since !== null && month >= since }
      months.set(month, m)
    }
    m.kinds[String(r[kind])] = {
      authorizations,
      refusalRate: r[rate] === null ? null : Number(r[rate]),
    }
  }
  return [...months.values()].sort((a, b) => b.month.localeCompare(a.month))
}

export interface NMIRefusal {
  kind: string
  category: string
  reason: string
  count: number
  // share is of every refusal in the range.
  share: number
}

export function nmiRefusals(result: MetricsResult | undefined): NMIRefusal[] {
  if (!result) return []
  const idx = indexColumns(result.columns)
  const col = (name: string) =>
    idx.dims.find((d) => d.name === name)?.index ?? -1
  const [kind, cat, rsn] = [col("nmi_kind"), col("category"), col("reason")]
  const count = idx.measures[0]?.index ?? -1
  if ([kind, cat, rsn, count].includes(-1)) return []
  const rows = result.rows
    .map((r) => ({
      kind: String(r[kind] ?? ""),
      category: String(r[cat] ?? ""),
      reason: String(r[rsn] ?? ""),
      count: Number(r[count] ?? 0),
      share: 0,
    }))
    .filter((r) => r.count > 0)
  const total = rows.reduce((sum, r) => sum + r.count, 0)
  return rows
    .map((r) => ({ ...r, share: total ? r.count / total : 0 }))
    .sort((a, b) => b.count - a.count)
}
