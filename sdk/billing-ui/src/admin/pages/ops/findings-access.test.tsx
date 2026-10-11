// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter } from "#orck/admin/test/router"
import { afterEach, beforeEach, expect, it, vi } from "vitest"

import { NotificationBell } from "#orck/admin/components/notification-bell"
import { CatalogDriftPage } from "#orck/admin/pages/catalog"
import { adminQueries } from "#orck/admin/test/harness"
import {
  FULL_ACCESS,
  calls,
  client,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "#orck/admin/test/harness"
import { act, browserEnvironment, mount, unmount } from "#orck/admin/test/mount"
import { OpsPage } from "."

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {
    "GET /merchant/findings": { data: [], next_cursor: null },
    "POST /metrics/query": { columns: [], rows: [] },
  }
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

it("billing and catalog staff never request the findings inbox, drift or bell", async () => {
  const queries = client()
  queries.setQueryData(adminQueries.access().queryKey, {
    ...FULL_ACCESS,
    merchant_config: false,
  })
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter>
        <OpsPage />
        <CatalogDriftPage />
        <NotificationBell />
      </MemoryRouter>
    </QueryClientProvider>
  )
  expect(document.querySelector("[aria-label=Notifications]")).toBeNull()
  expect(calls(requests)).not.toContain("GET /merchant/findings")
  expect(calls(requests)).not.toContain("POST /metrics/query")
  queries.clear()
})

it("a merchant-config operator can open finding resolution without billing or metrics access", async () => {
  const queries = client()
  const finding = {
    id: "fnd_operator",
    finding_type: "life.worker.stalled",
    subject_key: "worker",
    severity: "high",
    status: "requires_review",
    last_seen_at: "2026-10-11T00:00:00Z",
    resolved_at: null,
  }
  routes["GET /merchant/findings"] = { data: [finding], next_cursor: null }
  queries.setQueryData(adminQueries.access().queryKey, {
    ...FULL_ACCESS,
    admin: "none",
    catalog: false,
    metrics: false,
  })
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter>
        <OpsPage />
        <NotificationBell />
      </MemoryRouter>
    </QueryClientProvider>
  )
  await vi.waitFor(() => expect(document.body.textContent).toContain("Resolve"))
  expect(document.querySelector("[aria-label=Notifications]")).not.toBeNull()
  expect(calls(requests)).toContain("GET /merchant/findings")
  expect(calls(requests)).not.toContain("POST /metrics/query")
  await act(async () => {
    const resolve = [...document.querySelectorAll("button")].find(
      (button) => button.textContent === "Resolve"
    )!
    resolve.click()
  })
  expect(document.body.textContent).toContain("Resolve finding")
  queries.clear()
})

it("merchant configuration opens the catalog drift findings query", async () => {
  const queries = client()
  queries.setQueryData(adminQueries.access().queryKey, {
    ...FULL_ACCESS,
    admin: "none",
    catalog: false,
    metrics: false,
  })
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter>
        <CatalogDriftPage />
      </MemoryRouter>
    </QueryClientProvider>
  )
  await vi.waitFor(() =>
    expect(calls(requests)).toContain("GET /merchant/findings")
  )
  expect(
    new URLSearchParams(
      requests.find((request) => request.path === "/merchant/findings")!.query
    ).get("type")
  ).toBe("catalog.*")
  expect(document.body.textContent).not.toContain(
    "Catalog updates are unavailable"
  )
  queries.clear()
})
