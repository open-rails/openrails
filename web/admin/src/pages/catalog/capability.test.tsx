// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter } from "react-router-dom"
import { afterEach, expect, it } from "vitest"
import { client, selectMerchant, server } from "@/test/harness"
import { act, browserEnvironment, mount, unmount } from "@/test/mount"
import { CatalogProductsPage } from "./index"

afterEach(unmount)

it("keeps catalog reads visible but only offers creation where catalog edits are mounted", async () => {
  browserEnvironment()
  let enabled = false
  const requests = await server({
    "/config": () => ({
      capabilities: { route_groups: { catalog_write: enabled }, features: {} },
    }),
    "GET /admin/catalog/products": {
      items: [],
      total: 0,
      limit: 100,
      offset: 0,
    },
  })
  selectMerchant("merchant-one")
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter>
        <CatalogProductsPage />
      </MemoryRouter>
    </QueryClientProvider>
  )
  expect(document.body.textContent).toContain("No products yet")
  expect(document.body.textContent).toContain("Catalog updates are unavailable")
  expect(
    [...document.querySelectorAll("button")].some(
      (b) => b.textContent === "Apply catalog"
    )
  ).toBe(false)
  expect(requests.every((r) => r.method === "GET")).toBe(true)
  enabled = true
  await act(async () => {
    await queries.invalidateQueries()
  })
  expect(
    [...document.querySelectorAll("button")].some(
      (b) => b.textContent === "Apply catalog"
    )
  ).toBe(true)
})
