// @vitest-environment jsdom
// Reports mounted against the real API client: the daily and monthly tables
// come from metrics queries over the URL's range, newest first, and each
// downloads as CSV, oldest first.
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { afterEach, beforeEach, expect, it, vi } from "vitest"

import {
  client,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { act, browserEnvironment, mount, unmount } from "@/test/mount"

import { ReportsPage } from "./reports"

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

const count = (name: string) => ({ name, kind: "measure", unit: "count" })
const time = { name: "time", kind: "time" }

const answer = (body: { measures: string[]; grain: string }) => {
  if (body.grain === "day")
    return {
      grain: "day",
      columns: [
        time,
        count("new_subscriptions"),
        count("cancellations"),
        count("net_new_subscriptions"),
      ],
      rows: [
        ["2026-09-01T00:00:00Z", 2, 0, 2],
        ["2026-09-02T00:00:00Z", 1, 3, -2],
      ],
    }
  if (body.measures.includes("payment_count"))
    return {
      grain: "month",
      columns: [time, count("payment_count")],
      rows: [
        ["2026-08-01T00:00:00Z", 0],
        ["2026-09-01T00:00:00Z", 5],
      ],
    }
  return {
    grain: "month",
    columns: [
      time,
      count("cancellations"),
      { name: "churn_rate", kind: "measure", unit: "ratio" },
    ],
    rows: [
      ["2026-08-01T00:00:00Z", 0, null],
      ["2026-09-01T00:00:00Z", 3, 0.25],
    ],
  }
}

it("tables the daily and monthly reports and downloads each as CSV", async () => {
  routes["POST /metrics/query"] = (request) =>
    answer(request.body as { measures: string[]; grain: string })
  const files: { name: string; text: Promise<string> }[] = []
  let blob: Blob | undefined
  URL.createObjectURL = (b: Blob) => {
    blob = b
    return "blob:report"
  }
  URL.revokeObjectURL = () => {}
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement
  ) {
    files.push({ name: this.download, text: blob!.text() })
  })

  await mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter initialEntries={["/reports?from=2026-08-20&to=2026-09-02"]}>
        <Routes>
          <Route path="/reports" element={<ReportsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )
  const range = { from: "2026-08-20", to: "2026-09-02" }
  await vi.waitFor(() =>
    expect(document.querySelectorAll("tbody tr")).toHaveLength(4)
  )
  const bodies = requests
    .filter((r) => r.path === "/metrics/query")
    .map((r) => r.body)
  expect(bodies).toEqual(
    expect.arrayContaining([
      {
        measures: [
          "new_subscriptions",
          "cancellations",
          "net_new_subscriptions",
        ],
        by: ["time"],
        grain: "day",
        range,
      },
      {
        measures: ["payment_count"],
        by: ["time"],
        grain: "month",
        range,
        filters: { attempt_kind: ["renewal"] },
      },
      {
        measures: ["cancellations", "churn_rate"],
        by: ["time"],
        grain: "month",
        range,
      },
    ])
  )
  const rows = [...document.querySelectorAll("tbody tr")].map((tr) =>
    [...tr.querySelectorAll("td")].map((td) => td.textContent)
  )
  expect(rows).toEqual([
    ["Sep 2, 2026", "1", "3", "-2"],
    ["Sep 1, 2026", "2", "0", "+2"],
    ["Sep 2026", "5", "3", "25%"],
    ["Aug 2026", "0", "0", "—"],
  ])
  expect(document.querySelector("tfoot")?.textContent).toBe("Total330")

  const downloads = [
    ...document.querySelectorAll<HTMLButtonElement>("button"),
  ].filter((b) => b.textContent === "Download CSV")
  for (const button of downloads) await act(async () => button.click())
  expect(files.map((f) => f.name)).toEqual([
    "daily-subscriptions-2026-08-20-to-2026-09-02.csv",
    "monthly-renewals-2026-08-20-to-2026-09-02.csv",
  ])
  expect(await files[0].text).toBe(
    "date,new_subscriptions,cancellations,net_change\n2026-09-01,2,0,2\n2026-09-02,1,3,-2"
  )
  expect(await files[1].text).toBe(
    "month,renewals,cancellations,churn_rate\n2026-08,0,0,\n2026-09,5,3,0.25"
  )
})
