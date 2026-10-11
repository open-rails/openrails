import { AdminTestProvider as QueryClientProvider } from "#orck/admin/test/harness"
import { MemoryRouter } from "#orck/admin/test/router"
// @vitest-environment jsdom
// The subscription list mounted against the real API client: the views ask
// for their filters relative to today in their order, and the pager says
// which rows are shown, changes rows per page and goes back to the first page.

import { Route, Routes } from "react-router-dom"
import { afterEach, beforeEach, expect, it, vi } from "vitest"

import {
  client,
  cursorPages,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "#orck/admin/test/harness"
import {
  act,
  browserEnvironment,
  click,
  mount,
  unmount,
} from "#orck/admin/test/mount"

import { SubscriptionsPage } from "./index"
import { viewFilters } from "./views"

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

const sub = (i: number) => ({
  id: `sub_${i}`,
  customer_id: `cus_${i}`,
  status: "canceled",
  rail: "stripe",
  started_at: "2026-09-01T00:00:00Z",
  current_period_ends_at: "2026-10-01T00:00:00Z",
  canceled_at: "2026-09-20T00:00:00Z",
  dunning: null,
})

const open = (path: string) =>
  mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/subscriptions" element={<SubscriptionsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )
const listed = () =>
  requests.filter((r) => r.path === "/admin/subscriptions").map((r) => r.query)
const text = () => document.body.textContent ?? ""
const press = (label: string) =>
  act(async () =>
    document
      .querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`)!
      .click()
  )

it("names each view's filters relative to today", () => {
  const now = new Date("2026-10-11T23:30:00Z")
  expect(viewFilters("renewing_soon", now)).toEqual({
    status: "active",
    expires_before: "2026-10-18",
    sort: "current_period_ends_at",
  })
  expect(viewFilters("recently_canceled", now)).toEqual({
    canceled_after: "2026-09-11",
    sort: "canceled_at",
  })
  expect(viewFilters("bogus", now)).toBeNull()
})

it("lists a view in its order and pages it", async () => {
  routes["/admin/subscriptions"] = cursorPages(
    Array.from({ length: 60 }, (_, i) => sub(i)),
    50
  )
  await open("/subscriptions?view=recently_canceled")
  await vi.waitFor(() => expect(text()).toContain("Showing 1–50"))
  const since = viewFilters("recently_canceled")!.canceled_after
  expect(listed()).toEqual([
    `canceled_after=${since}&sort=canceled_at&limit=50`,
  ])
  expect(
    [...document.querySelectorAll("th")].map((th) => th.textContent)
  ).toContain("Canceled")

  await press("Next page")
  await vi.waitFor(() => expect(text()).toContain("Showing 51–60"))
  expect(listed().at(-1)).toBe(
    `canceled_after=${since}&sort=canceled_at&limit=50&cursor=50`
  )
  await press("First page")
  await vi.waitFor(() => expect(text()).toContain("Showing 1–50"))

  await press("Rows per page")
  const hundred = [
    ...document.querySelectorAll<HTMLElement>('[role="option"]'),
  ].find((option) => option.textContent === "100")!
  await act(async () => hundred.click())
  await vi.waitFor(() =>
    expect(listed().at(-1)).toBe(
      `canceled_after=${since}&sort=canceled_at&limit=100`
    )
  )

  await click("Renewing soon")
  await vi.waitFor(() =>
    expect(listed().at(-1)).toBe(
      `status=active&expires_before=${viewFilters("renewing_soon")!.expires_before}&sort=current_period_ends_at&limit=100`
    )
  )
})
