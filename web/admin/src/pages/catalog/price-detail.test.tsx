// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { afterEach, expect, it } from "vitest"

import {
  aPrice,
  aProduct,
  client,
  selectMerchant,
  server,
} from "@/test/harness"
import { browserEnvironment, mount, unmount } from "@/test/mount"
import { PriceDetailPage } from "./price-detail"

afterEach(unmount)

it("shows qualified price revisions while history links keep the immutable price ids", async () => {
  browserEnvironment()
  const current = aPrice("price_current", "prod_premium", {
    key: "monthly",
    revision: 1,
  })
  const previous = aPrice("price_previous", "prod_premium", {
    key: "monthly",
    revision: 0,
    archived: true,
  })
  await server({
    "/admin/catalog/revision": { revision: 2, writes_allowed: true },
    "/config": { capabilities: { route_groups: { catalog_write: true }, features: {} } },
    "/admin/catalog/prices/price_current": current,
    "/admin/catalog/products/prod_premium": aProduct("prod_premium", 0, {
      key: "premium",
      display_name: "Premium",
    }),
    "/admin/catalog/prices/price_current/history": {
      data: [current, previous].map((price) => ({
        price,
        effective_at: price.created_at,
        archived: price.archived,
      })),
      next_cursor: null,
    },
    "/admin/price-migrations": { data: [], next_cursor: null },
    "/admin/psps/routing-preview": { candidates: [] },
  })
  selectMerchant("merchant-a")
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter initialEntries={["/catalog/prices/price_current"]}>
        <Routes>
          <Route path="/catalog/prices/:id" element={<PriceDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )

  expect(document.querySelector("h2")?.textContent).toBe("premium.monthly.v1")
  expect(
    [...document.querySelectorAll<HTMLAnchorElement>("table a")].map(
      (link) => ({
        label: link.textContent,
        href: link.getAttribute("href"),
      })
    )
  ).toEqual([
    { label: "premium.monthly.v1", href: "/catalog/prices/price_current" },
    { label: "premium.monthly.v0", href: "/catalog/prices/price_previous" },
  ])
  queries.clear()
})
