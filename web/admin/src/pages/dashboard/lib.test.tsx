// Dashboard money: one currency is never compared, labelled or plotted as
// another, and no displayed amount goes through a JavaScript Number.
import { QueryObserver } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { MetricsResult } from "@/lib/api/metrics"
import { formatNativeAmount } from "@/lib/format"
import { adminQueries } from "@/lib/queries"
import { client, render, server, UNSAFE } from "@/test/harness"
import { ChartContainer } from "@/components/ui/chart"
import {
  donutSlices, exactKey, filteredCurrency, formatMeasure, groupSeries,
  pivotTimeSeries, statDelta,
} from "./lib"
import { MetricTooltip, WidgetVizView } from "./widget-viz"

const range = { from: "2026-09-01", to: "2026-09-03" }
const money = { name: "revenue", kind: "measure", unit: "money" } as const
const currency = { name: "currency", kind: "dimension" } as const
const time = { name: "bucket", kind: "time" } as const

const moneyStats: MetricsResult = {
  range,
  columns: [currency, money],
  rows: [["USD", "100000000"], ["JPY", 200_000_000]],
  // Deliberately reverse row order: matching is by dimensions, not position.
  compare_rows: [["JPY", "100000000"], ["USD", "100000000"]],
}
const moneyChart: MetricsResult = {
  range,
  grain: "day",
  columns: [time, currency, money],
  rows: [
    ["2026-09-01", "EUR", "100000000"], ["2026-09-01", "JPY", 200_000_000],
    ["2026-09-02", "EUR", 150_000_000], ["2026-09-02", "JPY", 250_000_000],
  ],
}
const oneCurrency: MetricsResult = {
  range, columns: [money], rows: [["100000000"]], compare_rows: [["100000000"]],
}

beforeEach(async () => {
  await server()
})
afterEach(() => vi.unstubAllGlobals())

describe("comparisons", () => {
  it("compares each currency against its own previous period", () => {
    expect(statDelta(moneyStats, moneyStats.rows[0], 1)).toBe(0)
    expect(statDelta(moneyStats, moneyStats.rows[1], 1)).toBe(1)
    const html = render(<WidgetVizView viz="stat" result={moneyStats} />)
    expect(html).toContain("+0.0% vs previous period")
    expect(html).toContain("+100.0% vs previous period")
    expect(html).not.toContain("+50.0%")
    expect(html).toContain(formatNativeAmount(100_000_000, "USD"))
    expect(html).toContain(formatNativeAmount(200_000_000, "JPY"))
  })

  it("matches whole dimension tuples without delimiter collisions", () => {
    const result: MetricsResult = {
      range,
      columns: [
        { name: "a", kind: "dimension" }, { name: "b", kind: "dimension" },
        { name: "count", kind: "measure", unit: "count" },
      ],
      rows: [["x · y", "z", 8], ["x", "y · z", 6]],
      compare_rows: [["x", "y · z", 3], ["x · y", "z", 8]],
    }
    expect(statDelta(result, result.rows[0], 2)).toBe(0)
    expect(statDelta(result, result.rows[1], 2)).toBe(1)
    const pivot = pivotTimeSeries({
      ...result,
      columns: [time, ...result.columns],
      rows: result.rows.map((row) => ["2026-09-01", ...row]),
    })
    expect(pivot.series[0].key).not.toBe(pivot.series[1].key)
    expect(pivot.data[0][pivot.series[0].key]).toBe(8)
    expect(pivot.data[0][pivot.series[1].key]).toBe(6)
  })

  it("invents no comparison from duplicate, missing or absent rows", () => {
    const duplicated = { ...moneyStats, rows: [...moneyStats.rows, moneyStats.rows[0]] }
    expect(statDelta(duplicated, duplicated.rows[0], 1)).toBeNull()
    expect(statDelta({ ...moneyStats, compare_rows: [["JPY", 0]] }, moneyStats.rows[1], 1)).toBeNull()
    expect(statDelta(moneyStats, ["USD", null], 1)).toBeNull()
    expect(statDelta(moneyStats, ["EUR", 20], 1)).toBeNull()
  })
})

describe("units", () => {
  it("keeps each currency in its own axis and tooltip unit", () => {
    const groups = groupSeries(pivotTimeSeries(moneyChart).series)
    expect(groups.map((group) => group.label)).toEqual(["EUR", "JPY"])
    for (const group of groups) {
      const series = group.series[0]
      expect(group.series).toHaveLength(1)
      expect(series.dimensions).toEqual([group.label])
      expect(formatMeasure("100000000", series.unit, series.currency)).toBe(
        formatNativeAmount(100_000_000, group.label)
      )
    }
    for (const viz of ["line", "area", "bar", "donut"] as const) {
      const html = render(
        <WidgetVizView
          viz={viz}
          result={viz === "donut" ? { ...moneyStats, rows: [["EUR", "100000000"], ["JPY", 200_000_000]] } : moneyChart}
        />
      )
      expect(html).toContain("EUR")
      expect(html).toContain("JPY")
      expect(html).not.toContain("$")
    }
  })

  it("keeps a filtered-out currency and leaves non-money measures alone", () => {
    const query = { measures: ["revenue"], range: { last: "7d" }, filters: { currency: ["EUR"] } }
    expect(filteredCurrency(query)).toBe("EUR")
    expect(filteredCurrency({ ...query, filters: { currency: ["EUR", "JPY"] } })).toBeUndefined()
    for (const viz of ["stat", "table"] as const) {
      const html = render(<WidgetVizView viz={viz} result={oneCurrency} query={query} />)
      expect(html).toContain(formatNativeAmount(100_000_000, "EUR"))
      expect(html).not.toContain("$")
    }
    expect(formatMeasure("100000000", "money")).not.toContain("$")
    expect([formatMeasure(1234, "count"), formatMeasure(0.25, "ratio"), formatMeasure(null, "count")]).toEqual(
      [(1234).toLocaleString(), "25%", "—"]
    )
    const mixed = pivotTimeSeries({
      ...moneyChart,
      columns: [...moneyChart.columns, { name: "count", kind: "measure", unit: "count" }],
      rows: moneyChart.rows.map((row) => [...row, 10]),
    })
    expect(groupSeries(mixed.series).map((group) => group.series[0].unit)).toEqual(["money", "count", "money"])
  })

  // A changed single-currency query has no currency dimension in its rows.
  // Keeping the old query's data would paint EUR money with a JPY label.
  it("does not relabel a stale result when the currency filter changes", async () => {
    const queries = client()
    const eur = { measures: ["revenue"], range: { last: "7d" }, filters: { currency: ["EUR"] } }
    const jpy = { ...eur, filters: { currency: ["JPY"] } }
    queries.setQueryData(adminQueries.widgetMetrics(eur).queryKey, oneCurrency)
    const observer = new QueryObserver(queries, { ...adminQueries.widgetMetrics(eur), staleTime: Infinity })
    const unsubscribe = observer.subscribe(() => {})
    try {
      const yen = { ...oneCurrency, rows: [[200_000_000]] }
      const options = { ...adminQueries.widgetMetrics(jpy), queryFn: async () => yen, staleTime: Infinity }
      observer.setOptions(options)
      expect(observer.getCurrentResult().data).toBeUndefined()
      await queries.fetchQuery(options)
      expect(observer.getCurrentResult().data).toEqual(yen)
    } finally {
      unsubscribe()
      observer.destroy()
      queries.clear()
    }
  })
})

describe("money beyond Number precision", () => {
  it("keeps the exact wire string in the stat, table and chart tooltip", () => {
    const { data, series } = pivotTimeSeries({
      grain: "day", range,
      columns: [{ name: "time", kind: "time" }, currency, money],
      rows: [["2026-09-01T00:00:00Z", "USD", UNSAFE]],
    })
    expect(formatMeasure(UNSAFE, "money", "USD")).toBe(formatNativeAmount(UNSAFE, "USD"))
    expect(data[0][exactKey(series[0].key)]).toBe(UNSAFE)
    expect(formatMeasure(String(data[0][exactKey(series[0].key)]), "money", "USD")).toBe(
      formatNativeAmount(UNSAFE, "USD")
    )
  })

  it("renders the donut tooltip from the exact cell, not the plotted Number", () => {
    const slices = donutSlices({ range, columns: [currency, money], rows: [["USD", UNSAFE]] })
    expect(slices[0][exactKey(slices[0].key)]).toBe(UNSAFE)
    expect(slices[0].value).toBe(9007199254740992) // the plotted Number is lossy
    const html = render(
      <ChartContainer config={{ [slices[0].key]: { label: "USD" } }}>
        <MetricTooltip
          series={slices}
          nameKey="key"
          active
          payload={[{
            name: slices[0].key, dataKey: "value", graphicalItemId: "pie",
            value: slices[0].value, payload: slices[0],
          }]}
        />
      </ChartContainer>
    )
    expect(html).toContain(formatNativeAmount(UNSAFE, "USD"))
    expect(html).toContain("9,007,199,254.740993")
    expect(html).not.toContain(formatNativeAmount(slices[0].value, "USD"))
  })
})
