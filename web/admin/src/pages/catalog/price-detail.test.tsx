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

it("shows qualified price versions while history links keep the immutable price ids", async () => {
  browserEnvironment()
  const current = aPrice("premium:monthly:v2", "prod_premium", {
    key: "monthly",
    version: 2,
    revision: 3,
  })
  const previous = aPrice("premium:monthly:v1", "prod_premium", {
    key: "monthly",
    version: 1,
    revision: 3,
    archived: true,
  })
  await server({
    "/config": {
      capabilities: { route_groups: { catalog_write: true }, features: {} },
    },
    "/catalog/prices/premium%3Amonthly%3Av2": current,
    "/catalog/products/prod_premium": aProduct("prod_premium", 0, {
      key: "premium",
      display_name: "Premium",
    }),
    "/catalog/prices/premium%3Amonthly%3Av2/history": {
      data: [current, previous].map((price) => ({
        price,
        effective_at: price.created_at,
        archived: price.archived,
      })),
      next_cursor: null,
    },
    "/catalog/price-migrations": { data: [], next_cursor: null },
    "/merchant/psps/routing-preview": { candidates: [] },
  })
  selectMerchant("merchant-a")
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter initialEntries={["/catalog/prices/premium%3Amonthly%3Av2"]}>
        <Routes>
          <Route path="/catalog/prices/:id" element={<PriceDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )

  expect(document.querySelector("h2")?.textContent).toBe("premium:monthly:v2")
  expect(
    [...document.querySelectorAll<HTMLAnchorElement>("table a")].map(
      (link) => ({
        label: link.textContent,
        href: link.getAttribute("href"),
      })
    )
  ).toEqual([
    {
      label: "premium:monthly:v2",
      href: "/catalog/prices/premium%3Amonthly%3Av2",
    },
    {
      label: "premium:monthly:v1",
      href: "/catalog/prices/premium%3Amonthly%3Av1",
    },
  ])
  queries.clear()
})
