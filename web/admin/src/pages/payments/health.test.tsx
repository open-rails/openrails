// Payments → Health (#1117): the queries the page sends, how it shapes the
// answers and where each tile and cell leads. The fixtures are the metrics and
// list responses of the #1116 greenfield scenario (a new card declined for its
// CVV then approved; a renewal declined twice and collected on the second
// retry), captured with each query that produced them.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  ATTEMPT_FILTERS,
  CYCLE_FILTERS,
  filtersFrom,
} from "@/lib/api/endpoints"
import type { MetricsQuery, MetricsResult } from "@/lib/api/metrics"
import { pivotTimeSeries } from "@/pages/dashboard/lib"
import { adminQueries } from "@/lib/queries"
import {
  client,
  render,
  selectMerchant,
  server,
  type Recorded,
} from "@/test/harness"
import fixtures from "./fixtures/health.json"
import { PaymentHealthPage } from "./health"
import {
  checkQueries,
  checkRows,
  coverageQuery,
  coverageRows,
  formatChange,
  listURL,
  missedQuery,
  missedRows,
  reasonGroups,
  reasonsQuery,
  recoveryCurves,
  recoveryQueries,
  TILES,
  tileValue,
  trendQueries,
  type Scope,
} from "./health-model"

type Captured = { query: MetricsQuery; result: MetricsResult }
const captured = fixtures.metrics as unknown as Record<string, Captured>
const result = (name: string) => captured[name].result

const all: Scope = { owner: "", last: "30d" }
const tile = (id: string) => TILES.find((t) => t.id === id)!

// The page's query for each captured one; the capture pins an absolute range
// because the scenario runs on a test clock.
const builders = (scope: Scope): Record<string, MetricsQuery> => ({
  tileNewCard: tile("new-card-decline").query(scope),
  tileCheckout: tile("checkout-failure").query(scope),
  tileCycles: tile("rebill-first-failure").query(scope),
  trendNewCard: trendQueries(scope).newCard,
  trendRebill: trendQueries(scope).rebill,
  reasons: reasonsQuery(scope),
  avs: checkQueries(scope).avs,
  cvv: checkQueries(scope).cvv,
  recoveryRate: recoveryQueries(scope).rate,
  byAttempt: recoveryQueries(scope).byAttempt,
  byDays: recoveryQueries(scope).byDays,
  missed: missedQuery(scope),
  coverage: coverageQuery(scope),
})
const wire = (q: MetricsQuery) => JSON.parse(JSON.stringify(q))

let requests: Recorded[]
beforeEach(async () => {
  requests = await server()
  selectMerchant("merchant-a")
})
afterEach(() => vi.unstubAllGlobals())

describe("queries", () => {
  it("sends the captured queries", () => {
    const built = builders(all)
    expect(Object.keys(built).sort()).toEqual(Object.keys(captured).sort())
    for (const [name, { query }] of Object.entries(captured))
      expect(wire({ ...built[name], range: query.range }), name).toEqual(query)
  })

  it("shares one query across the cycle tiles", () => {
    const cycleTiles = TILES.filter((t) => t.list === "cycles")
    expect(cycleTiles).toHaveLength(4)
    for (const t of cycleTiles)
      expect(t.query(all)).toEqual(tile("rebill-first-failure").query(all))
  })

  it("scopes every query to the owner and PSP account", () => {
    const scope: Scope = {
      owner: "nmi_schedule",
      psp: { id: "psp-1", account: "acct-1" },
      last: "7d",
    }
    for (const [name, q] of Object.entries(builders(scope))) {
      expect(q.filters?.owner, name).toEqual(["nmi_schedule"])
      expect(q.filters?.rail_account, name).toEqual(["acct-1"])
    }
    expect(tile("missed").query(scope).range).toEqual({ last: "7d" })
    expect(trendQueries(scope).rebill.range).toEqual({ last: "12w" })
  })
})

describe("shaping", () => {
  it("reads a tile and its move against the previous period", () => {
    const newCard = tileValue(result("tileNewCard"), tile("new-card-decline"))!
    expect(newCard.value).toBeCloseTo(1 / 3)
    expect(formatChange(newCard)).toBe("no previous period")
    const missed = tileValue(result("tileCycles"), tile("missed"))!
    expect([missed.value, missed.change, missed.worse]).toEqual([0, 0, false])
    expect(tileValue(result("tileCycles"), tile("recovery"))!.value).toBe(1)
    const worse = tileValue(
      { ...result("tileNewCard"), compare_rows: [[0.25]] },
      tile("new-card-decline")
    )!
    expect(worse.worse).toBe(true)
    expect(formatChange(worse)).toBe("+8.3 pp vs previous period")
    const better = tileValue(
      { ...result("tileCycles"), compare_rows: [[1, 0, 0.5, 1]] },
      tile("recovery")
    )!
    expect([better.change, better.worse]).toEqual([0.5, false])
  })

  it("leaves a week with no attempts out of the trend rather than at 0%", () => {
    const { data, series } = pivotTimeSeries(result("trendNewCard"))
    expect(series.map((s) => s.label)).toEqual(["engine"])
    expect(data.map((d) => d[series[0].key])).toEqual([
      1 / 3, null, null, null, null, null,
    ])
  })

  it("nests declines by category, reason and code, largest first", () => {
    const groups = reasonGroups(result("reasons"))
    expect(groups.map((g) => [g.category, g.count])).toEqual([
      ["issuer_soft", 2],
      ["card_data", 1],
    ])
    expect(groups[0].share).toBeCloseTo(2 / 3)
    expect(groups[1].rows[0]).toMatchObject({
      reason: "incorrect_cvc",
      code: "200",
      count: 1,
      previous: 0,
    })
  })

  it("reads new-card attempts by CVV result", () => {
    expect(checkRows(result("cvv"))).toEqual([
      { result: "", attempts: 2, failureRate: 0 },
      { result: "N", attempts: 1, failureRate: 1 },
    ])
  })

  it("turns recoveries into cumulative shares of failed cycles", () => {
    expect(
      recoveryCurves(
        result("recoveryRate"),
        result("byAttempt"),
        "recovery_attempt"
      )
    ).toEqual([{ owner: "engine", points: [0, 0, 1, 1, 1] }])
    expect(
      recoveryCurves(result("recoveryRate"), result("byDays"), "days_to_recover")
    ).toEqual([{ owner: "engine", points: [0, 0, 1, 1, 1] }])
    // Half the failed cycles recovered: two recoveries mean four failures.
    const half: MetricsResult = {
      ...result("recoveryRate"),
      rows: [["nmi_schedule", 0.5, 2]],
    }
    const steps: MetricsResult = {
      ...result("byAttempt"),
      rows: [
        ["nmi_schedule", "1", 1],
        ["nmi_schedule", "5+", 1],
      ],
    }
    expect(recoveryCurves(half, steps, "recovery_attempt")).toEqual([
      { owner: "nmi_schedule", points: [0.25, 0.25, 0.25, 0.25, 0.5] },
    ])
  })

  it("reads missed rebills and webhook coverage", () => {
    expect(missedRows(result("missed"))).toEqual([])
    expect(coverageRows(result("coverage"))).toEqual([])
    const coverage: MetricsResult = {
      ...result("coverage"),
      rows: [
        ["acct-1", "webhook", 3],
        ["acct-1", "pull", 1],
      ],
    }
    expect(coverageRows(coverage)).toEqual([
      { account: "acct-1", webhook: 3, pull: 1, coverage: 0.75 },
    ])
  })
})

describe("drill-down", () => {
  it("opens the attempt list the API filters the same way", async () => {
    const range = result("tileNewCard").range
    const url = listURL(
      "attempts",
      tile("new-card-decline").listFilters,
      { owner: "engine", psp: { id: "psp-1", account: "acct-1" }, last: "30d" },
      range
    )
    const params = new URL(url, "http://console").searchParams
    expect(url.startsWith("/payments/attempts?")).toBe(true)
    await client().fetchQuery(
      adminQueries.attempts(filtersFrom(params, ATTEMPT_FILTERS), 50, 0)
    )
    const sent = new URLSearchParams(requests.at(-1)!.query)
    expect(requests.at(-1)!.path).toBe("/merchant/payment-attempts")
    expect(Object.fromEntries(sent)).toEqual({
      kind: "verify,initial",
      card_entry: "new",
      category: "card_data,issuer_soft,issuer_hard,gateway_rule,system_error,unknown",
      owner: "engine",
      psp_id: "psp-1",
      since: range.from,
      until: range.to,
      limit: "50",
      offset: "0",
    })
  })

  it("opens the cycle list by due date", async () => {
    const range = result("tileCycles").range
    const url = listURL("cycles", tile("recovery").listFilters, all, range)
    const params = new URL(url, "http://console").searchParams
    await client().fetchQuery(
      adminQueries.cycles(filtersFrom(params, CYCLE_FILTERS), 50, 0)
    )
    expect(requests.at(-1)!.path).toBe("/merchant/rebill-cycles")
    expect(Object.fromEntries(new URLSearchParams(requests.at(-1)!.query)))
      .toMatchObject({
        first_outcome: "declined,error,missed",
        outcome: "collected",
        due_since: range.from,
        due_until: range.to,
      })
  })

  it("renders the tiles and reasons with their links", () => {
    const queries = client()
    queries.setQueryData(adminQueries.paymentProviders().queryKey, {
      data: [],
      provider_definitions: [],
    })
    const built = builders(all)
    for (const [name, { result }] of Object.entries(captured))
      queries.setQueryData(
        adminQueries.widgetMetrics(built[name]).queryKey,
        result
      )
    const html = render(<PaymentHealthPage />, queries)
    expect(html).toContain("33.33%")
    expect(html).toContain("incorrect_cvc")
    expect(html).toContain(
      `href="/payments/cycles?first_outcome=missed&amp;due_since=`
    )
    expect(html).toContain(
      `href="/payments/attempts?category=issuer_soft&amp;reason=insufficient_funds&amp;since=`
    )
  })
})
