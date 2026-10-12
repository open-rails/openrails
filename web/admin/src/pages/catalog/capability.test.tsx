// @vitest-environment jsdom
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter } from "react-router-dom"
import { afterEach, expect, it } from "vitest"
import { aProduct, client, selectMerchant, server } from "@/test/harness"
import { act, browserEnvironment, mount, unmount } from "@/test/mount"
import { CatalogProductsPage } from "./index"

afterEach(unmount)

const mountProducts = async () => {
  selectMerchant("merchant-one")
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <MemoryRouter>
        <CatalogProductsPage />
      </MemoryRouter>
    </QueryClientProvider>
  )
  return queries
}

const button = (label: string) =>
  [...document.querySelectorAll("button")].find((b) => b.textContent === label)

// Catalog access is the caller's /admin/access; no catalog-wide revision is read.
it("offers creation only to a caller holding the catalog", async () => {
  browserEnvironment()
  let enabled = false
  const requests = await server({
    "GET /admin/access": () => ({
      admin: "none",
      catalog: enabled ? "update" : "read",
      merchant_config: "none",
      metrics: false,
    }),
    "GET /catalog/products": { data: [], next_cursor: null },
  })
  const queries = await mountProducts()
  expect(document.body.textContent).toContain("No products yet")
  expect(document.body.textContent).toContain("Catalog updates are unavailable")
  expect(button("Apply catalog")).toBeUndefined()
  expect(requests.every((r) => r.method === "GET")).toBe(true)
  enabled = true
  await act(async () => {
    await queries.invalidateQueries()
  })
  expect(button("Apply catalog")).toBeDefined()
  expect(
    requests.some((r) => r.path.startsWith("/catalog/revision"))
  ).toBe(false)
})

// An edit sends the revision its product read as expected_revision.
it("edits a product at the revision it read", async () => {
  browserEnvironment()
  const requests = await server({
    "GET /catalog/products": {
      data: [
        aProduct("prod_pro", 0, {
          key: "pro",
          display_name: "Pro",
          revision: 7,
        }),
      ],
      next_cursor: null,
    },
  })
  await mountProducts()
  await act(async () => {
    button("Deactivate")!.click()
  })
  const patch = requests.find((r) => r.method === "PATCH")
  expect(patch?.path).toBe("/catalog/products/prod_pro")
  expect(patch?.body).toEqual({ archived: true, expected_revision: 7 })
  expect(
    requests.some((r) => r.path.startsWith("/catalog/revision"))
  ).toBe(false)
})
