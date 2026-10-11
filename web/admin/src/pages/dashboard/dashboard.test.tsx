// @vitest-environment jsdom
// The dashboard mounted against the real API client: every widget runs over
// the range in the URL, a fixed-range widget keeps its own, a long custom
// range coarsens a daily chart, and a one-day stat compares with yesterday.
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { afterEach, beforeEach, expect, it, vi } from "vitest"

import type { Widget } from "@/lib/api/metrics"
import {
  client,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { browserEnvironment, mount, unmount } from "@/test/mount"

import { DashboardPage } from "./index"
import {
  fitGrain,
  rangeDates,
  rangeFromParams,
  withRange,
  widgetQuery,
} from "./range"

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

const widget = (id: string, over: Partial<Widget>): Widget => ({
  id,
  title: id,
  viz: "stat",
  query: { measures: ["new_subscriptions"], range: { last: "30d" } },
  grid: { x: 0, y: 0, w: 3, h: 2 },
  fixed_range: false,
  ...over,
})

const today = widget("today-new", {
  title: "New subscriptions today",
  fixed_range: true,
  query: {
    measures: ["new_subscriptions"],
    range: { last: "1d" },
    compare: "previous_period",
  },
})
const daily = widget("new-vs-canceled", {
  viz: "line",
  query: {
    measures: ["new_subscriptions", "cancellations"],
    by: ["time"],
    grain: "day",
    range: { last: "30d" },
  },
})

const open = (path: string) =>
  mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/" element={<DashboardPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )
const queried = () =>
  requests
    .filter((r) => r.path === "/metrics/query")
    .map((r) => r.body as Record<string, unknown>)

it("runs every widget over the URL's range but a fixed one over its own", async () => {
  routes["/metrics/dashboard"] = { widgets: [today, daily], is_default: true }
  routes["POST /metrics/query"] = (request) => {
    const body = request.body as { range: { last?: string } }
    if (body.range.last === "1d")
      return {
        range: { from: "2026-10-11T00:00:00Z", to: "2026-10-12T00:00:00Z" },
        columns: [
          { name: "new_subscriptions", kind: "measure", unit: "count" },
        ],
        rows: [[3]],
        compare_range: {
          from: "2026-10-10T00:00:00Z",
          to: "2026-10-11T00:00:00Z",
        },
        compare_rows: [[2]],
      }
    return { range: body.range, columns: [], rows: [] }
  }
  await open("/?from=2024-01-01&to=2026-09-30")
  await vi.waitFor(() => expect(queried()).toHaveLength(2))
  expect(document.body.textContent).not.toContain("Add widget")
  expect(document.body.textContent).not.toContain("Add as widget")
  expect(document.querySelector("textarea")).toBeNull()
  expect(queried()).toContainEqual(today.query)
  expect(queried()).toContainEqual({
    ...daily.query,
    range: { from: "2024-01-01", to: "2026-09-30" },
    grain: "week",
  })
  await vi.waitFor(() =>
    expect(document.body.textContent).toContain("+50.0% vs yesterday")
  )
})

it("reads and writes the range in the URL", () => {
  expect(rangeFromParams(new URLSearchParams(""))).toEqual({ preset: "30d" })
  expect(rangeFromParams(new URLSearchParams("range=7d"))).toEqual({
    preset: "7d",
  })
  expect(rangeFromParams(new URLSearchParams("range=2w"))).toEqual({
    preset: "30d",
  })
  expect(
    rangeFromParams(new URLSearchParams("from=2026-09-30&to=2026-09-01"))
  ).toEqual({ from: "2026-09-01", to: "2026-09-30" })
  expect(
    withRange(new URLSearchParams("tab=x&range=7d"), {
      from: "2026-09-01",
      to: "2026-09-30",
    }).toString()
  ).toBe("tab=x&from=2026-09-01&to=2026-09-30")
  expect(
    withRange(new URLSearchParams("from=a&to=b"), { preset: "30d" }).toString()
  ).toBe("")

  const now = new Date("2026-10-11T15:00:00Z")
  expect(rangeDates({ preset: "7d" }, now)).toEqual({
    from: "2026-10-05",
    to: "2026-10-11",
  })
  expect(rangeDates({ preset: "6m" }, now)).toEqual({
    from: "2026-04-12",
    to: "2026-10-11",
  })
  expect(fitGrain("day", { from: "2026-01-01", to: "2026-12-31" }, now)).toBe(
    "day"
  )
  expect(fitGrain("day", { from: "2020-01-01", to: "2026-12-31" }, now)).toBe(
    "week"
  )
  expect(widgetQuery(today, { preset: "7d" })).toBe(today.query)
})
