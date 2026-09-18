import { renderToStaticMarkup } from "react-dom/server"
import { afterEach, describe, expect, it, vi } from "vitest"
import { QueryClient, QueryObserver } from "@tanstack/react-query"
import { adminQueries } from "@/lib/queries"
import type { MetricsResult } from "@/lib/api/metrics"
import { formatNativeAmount } from "@/lib/format"
import {
  donutSlices,
  filteredCurrency,
  formatMeasure,
  groupSeries,
  indexColumns,
  exactKey,
  pivotTimeSeries,
  statDelta,
} from "./lib"
import { ChartContainer } from "@/components/ui/chart"
import { MetricTooltip, WidgetVizView } from "./widget-viz"

afterEach(() => vi.unstubAllGlobals())

const range = { from: "2026-09-01", to: "2026-09-03" }
const moneyStats: MetricsResult = {
  range,
  columns: [
    { name: "currency", kind: "dimension" },
    { name: "revenue", kind: "measure", unit: "money" },
  ],
  rows: [
    ["USD", "100000000"],
    ["JPY", 200_000_000],
  ],
  // Deliberately reverse row order: matching is by dimensions, not array index.
  compare_rows: [
    ["JPY", "100000000"],
    ["USD", "100000000"],
  ],
}
const moneyChart: MetricsResult = {
  range,
  grain: "day",
  columns: [{ name: "bucket", kind: "time" }, ...moneyStats.columns],
  rows: [
    ["2026-09-01", "EUR", "100000000"],
    ["2026-09-01", "JPY", 200_000_000],
    ["2026-09-02", "EUR", 150_000_000],
    ["2026-09-02", "JPY", 250_000_000],
  ],
}

describe("dashboard currency", () => {
  it("compares unchanged USD and doubled JPY independently", () => {
    expect(statDelta(moneyStats, moneyStats.rows[0], 1)).toBe(0)
    expect(statDelta(moneyStats, moneyStats.rows[1], 1)).toBe(1)
    const html = renderToStaticMarkup(
      <WidgetVizView viz="stat" result={moneyStats} />
    )
    expect(html).toContain("+0.0% vs previous period")
    expect(html).toContain("+100.0% vs previous period")
    expect(html).not.toContain("+50.0%")
    expect(html).toContain(formatNativeAmount(100_000_000, "USD"))
    expect(html).toContain(formatNativeAmount(200_000_000, "JPY"))
  })

  it("matches all dimension tuples without delimiter collisions", () => {
    const result: MetricsResult = {
      range,
      columns: [
        { name: "a", kind: "dimension" },
        { name: "b", kind: "dimension" },
        { name: "count", kind: "measure", unit: "count" },
      ],
      rows: [
        ["x · y", "z", 8],
        ["x", "y · z", 6],
      ],
      compare_rows: [
        ["x", "y · z", 3],
        ["x · y", "z", 8],
      ],
    }
    expect(statDelta(result, result.rows[0], 2)).toBe(0)
    expect(statDelta(result, result.rows[1], 2)).toBe(1)
    const pivot = pivotTimeSeries({
      ...result,
      columns: [{ name: "bucket", kind: "time" }, ...result.columns],
      rows: result.rows.map((row) => ["2026-09-01", ...row]),
    })
    expect(pivot.series).toHaveLength(2)
    expect(pivot.series[0].key).not.toBe(pivot.series[1].key)
    expect(pivot.data[0][pivot.series[0].key]).toBe(8)
    expect(pivot.data[0][pivot.series[1].key]).toBe(6)
  })

  it("keeps EUR and JPY in separate axis and tooltip units", () => {
    const pivot = pivotTimeSeries(moneyChart)
    const groups = groupSeries(pivot.series)
    expect(groups.map((group) => group.label)).toEqual(["EUR", "JPY"])
    for (const group of groups) {
      expect(group.series).toHaveLength(1)
      const series = group.series[0]
      expect(series.dimensions).toEqual([group.label])
      expect(formatMeasure("100000000", series.unit, series.currency)).toBe(
        formatNativeAmount(100_000_000, group.label)
      )
      expect(
        formatMeasure("100000000", series.unit, series.currency)
      ).not.toContain("$")
    }
    for (const viz of ["line", "area", "bar", "donut"] as const) {
      const html = renderToStaticMarkup(
        <WidgetVizView
          viz={viz}
          result={
            viz === "donut"
              ? {
                  ...moneyStats,
                  rows: [
                    ["EUR", "100000000"],
                    ["JPY", 200_000_000],
                  ],
                }
              : moneyChart
          }
        />
      )
      expect(html).toContain("EUR")
      expect(html).toContain("JPY")
      expect(html).not.toContain("$")
    }
  })

  it("preserves a known currency when the query filters out its dimension", () => {
    const query = {
      measures: ["revenue"],
      range: { last: "7d" },
      filters: { currency: ["EUR"] },
    }
    expect(filteredCurrency(query)).toBe("EUR")
    const result: MetricsResult = {
      range,
      columns: [{ name: "revenue", kind: "measure", unit: "money" }],
      rows: [["100000000"]],
      compare_rows: [["100000000"]],
    }
    for (const viz of ["stat", "table"] as const) {
      const html = renderToStaticMarkup(
        <WidgetVizView viz={viz} result={result} query={query} />
      )
      expect(html).toContain(formatNativeAmount(100_000_000, "EUR"))
      expect(html).not.toContain("$")
    }
    expect(formatMeasure("100000000", "money")).not.toContain("$")
    expect(
      filteredCurrency({ ...query, filters: { currency: ["EUR", "JPY"] } })
    ).toBeUndefined()
  })

  it("separates incompatible units and leaves currency-free counts unchanged", () => {
    const result: MetricsResult = {
      range,
      columns: [{ name: "count", kind: "measure", unit: "count" }],
      rows: [[10]],
      compare_rows: [[5]],
    }
    expect(statDelta(result, result.rows[0], 0)).toBe(1)
    expect(formatMeasure(1234, "count")).toBe((1234).toLocaleString())
    expect(formatMeasure(0.25, "ratio")).toBe("25%")
    expect(formatMeasure(null, "count")).toBe("—")
    const mixed = pivotTimeSeries({
      ...moneyChart,
      columns: [
        ...moneyChart.columns,
        { name: "count", kind: "measure", unit: "count" },
      ],
      rows: moneyChart.rows.map((row) => [...row, 10]),
    })
    expect(
      groupSeries(mixed.series).map((group) => group.series[0].unit)
    ).toEqual(["money", "count", "money"])
    expect(indexColumns(result.columns).currency).toBe(-1)
  })

  it("does not invent a comparison by summing duplicate tuples or missing data", () => {
    const result = {
      ...moneyStats,
      rows: [...moneyStats.rows, moneyStats.rows[0]],
    }
    expect(statDelta(result, result.rows[0], 1)).toBeNull()
    expect(
      statDelta(
        { ...moneyStats, compare_rows: [["JPY", 0]] },
        moneyStats.rows[1],
        1
      )
    ).toBeNull()
    expect(statDelta(moneyStats, ["USD", null], 1)).toBeNull()
    expect(statDelta(moneyStats, ["EUR", 20], 1)).toBeNull()
  })
})

// A changed single-currency query has no currency dimension in its rows. Keeping
// the old query's data would paint EUR money with the new JPY label while loading.
it("does not relabel stale results when the currency filter changes", async () => {
  vi.stubGlobal("localStorage", { removeItem: vi.fn() })
  vi.stubGlobal("sessionStorage", { getItem: () => null })
  const client = new QueryClient()
  const eur = {
    measures: ["revenue"],
    range: { last: "7d" },
    filters: { currency: ["EUR"] },
  }
  const jpy = { ...eur, filters: { currency: ["JPY"] } }
  const euroResult: MetricsResult = {
    range,
    columns: [{ name: "revenue", kind: "measure", unit: "money" }],
    rows: [["100000000"]],
  }
  client.setQueryData(adminQueries.widgetMetrics(eur).queryKey, euroResult)
  const observer = new QueryObserver(client, {
    ...adminQueries.widgetMetrics(eur),
    staleTime: Infinity,
  })
  const unsubscribe = observer.subscribe(() => {})
  try {
    const yenResult = { ...euroResult, rows: [[200_000_000]] }
    const options = {
      ...adminQueries.widgetMetrics(jpy),
      queryFn: async () => yenResult,
      staleTime: Infinity,
    }
    observer.setOptions(options)
    expect(observer.getCurrentResult().data).toBeUndefined()
    await client.fetchQuery(options)
    expect(observer.getCurrentResult().data).toEqual(yenResult)
  } finally {
    unsubscribe()
    observer.destroy()
    client.clear()
  }
})

describe("money cells beyond Number precision", () => {
  it("keeps the exact wire string for stat, table and chart tooltips", () => {
    const huge = "9007199254740993" // 2^53 + 1: a Number cannot hold it
    const result: MetricsResult = {
      grain: "day",
      range,
      columns: [
        { name: "time", kind: "time" },
        { name: "currency", kind: "dimension" },
        { name: "revenue", kind: "measure", unit: "money" },
      ],
      rows: [["2026-09-01T00:00:00Z", "USD", huge]],
    }
    expect(formatMeasure(huge, "money", "USD")).toBe(
      formatNativeAmount(huge, "USD")
    )
    const { data, series } = pivotTimeSeries(result)
    expect(series).toHaveLength(1)
    expect(data[0][exactKey(series[0].key)]).toBe(huge)
    expect(
      formatMeasure(String(data[0][exactKey(series[0].key)]), "money", "USD")
    ).toBe(formatNativeAmount(huge, "USD"))
  })

  it("renders the donut tooltip from the exact slice cell, not the plotted Number", () => {
    const huge = "9007199254740993"
    const result: MetricsResult = {
      range,
      columns: [
        { name: "currency", kind: "dimension" },
        { name: "revenue", kind: "measure", unit: "money" },
      ],
      rows: [["USD", huge]],
    }
    const slices = donutSlices(result)
    expect(slices).toHaveLength(1)
    expect(slices[0][exactKey(slices[0].key)]).toBe(huge)
    expect(slices[0].value).toBe(9007199254740992) // the plotted Number is lossy
    const html = renderToStaticMarkup(
      <ChartContainer config={{ [slices[0].key]: { label: "USD" } }}>
        <MetricTooltip
          series={slices}
          nameKey="key"
          active
          payload={[
            {
              name: slices[0].key,
              dataKey: "value",
              graphicalItemId: "pie",
              value: slices[0].value,
              payload: slices[0],
            },
          ]}
        />
      </ChartContainer>
    )
    expect(html).toContain(formatNativeAmount(huge, "USD"))
    expect(html).toContain("9,007,199,254.740993")
    expect(html).not.toContain(formatNativeAmount(slices[0].value, "USD"))
  })
})
