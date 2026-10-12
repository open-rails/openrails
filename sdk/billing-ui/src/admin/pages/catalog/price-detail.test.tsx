import { AdminTestProvider as QueryClientProvider } from "#orck/admin/test/harness"
import { MemoryRouter } from "#orck/admin/test/router"
// @vitest-environment jsdom

import { Route, Routes } from "react-router-dom"
import { afterEach, expect, it } from "vitest"

import {
  aPrice,
  aProduct,
  client,
  selectMerchant,
  server,
} from "#orck/admin/test/harness"
import { act, browserEnvironment, mount, unmount } from "#orck/admin/test/mount"
import { PriceDetailPage } from "./price-detail"

afterEach(unmount)

it.each([
  ["premium", "monthly", "premium:monthly"],
  ["book/100%", "buy:USD", "book%2F100%25:buy%3AUSD"],
])(
  "keeps immutable price IDs through detail and history routing for %s",
  async (productKey, priceKey, prefix) => {
    browserEnvironment()
    const current = aPrice(`${prefix}:v2`, "prod_premium", {
      key: priceKey,
      version: 2,
      revision: 3,
    })
    const previous = aPrice(`${prefix}:v1`, "prod_premium", {
      key: priceKey,
      version: 1,
      revision: 3,
      archived: true,
    })
    const pricePath = (id: string) =>
      `/catalog/prices/${encodeURIComponent(id)}`
    const history = {
      data: [current, previous].map((price) => ({
        price,
        effective_at: price.created_at,
        archived: price.archived,
      })),
      next_cursor: null,
    }
    const requests = await server({
      "/config": {
        capabilities: { route_groups: { catalog_write: true }, features: {} },
      },
      [pricePath(current.id)]: current,
      [pricePath(previous.id)]: previous,
      "/catalog/products/prod_premium": aProduct("prod_premium", 0, {
        key: productKey,
        display_name: "Premium",
      }),
      [pricePath(current.id) + "/history"]: history,
      [pricePath(previous.id) + "/history"]: history,
      "/catalog/price-migrations": { data: [], next_cursor: null },
      "/merchant/psps/routing-preview": { candidates: [] },
    })
    selectMerchant("merchant-a")
    const queries = client()
    await mount(
      <QueryClientProvider client={queries}>
        <MemoryRouter initialEntries={[pricePath(current.id)]}>
          <Routes>
            <Route path="/catalog/prices/:id" element={<PriceDetailPage />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    )

    expect(document.querySelector("h2")?.textContent).toBe(current.id)
    expect(
      [...document.querySelectorAll<HTMLAnchorElement>("table a")].map(
        (link) => ({
          label: link.textContent,
          href: link.getAttribute("href"),
        })
      )
    ).toEqual([
      {
        label: current.id,
        href: pricePath(current.id),
      },
      {
        label: previous.id,
        href: pricePath(previous.id),
      },
    ])
    await act(async () =>
      document.querySelectorAll<HTMLAnchorElement>("table a")[1]!.click()
    )
    expect(document.querySelector("h2")?.textContent).toBe(
      previous.id + "archived"
    )
    expect(
      requests
        .filter(
          (request) =>
            request.method === "GET" &&
            request.path.startsWith("/catalog/prices/")
        )
        .map((request) => request.path)
    ).toEqual([
      pricePath(current.id),
      pricePath(current.id) + "/history",
      pricePath(previous.id),
      pricePath(previous.id) + "/history",
    ])
    queries.clear()
  }
)

it("refuses a malformed price URL without requesting a price", async () => {
  browserEnvironment()
  const requests = await server()
  selectMerchant("merchant-a")
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter initialEntries={["/catalog/prices/bad%ZZ"]}>
        <Routes>
          <Route path="/catalog/prices/:id" element={<PriceDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  )
  expect(document.querySelector('[role="alert"]')?.textContent).toBe(
    "Invalid price URL."
  )
  expect(
    requests.some((request) => request.path.startsWith("/catalog/prices/"))
  ).toBe(false)
  queries.clear()
})
