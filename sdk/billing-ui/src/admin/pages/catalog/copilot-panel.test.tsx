// @vitest-environment jsdom
import { AdminTestProvider as QueryClientProvider } from "#orck/admin/test/harness"
import { afterEach, expect, it } from "vitest"
import {
  aPrice,
  aProduct,
  adminQueries,
  client,
  FULL_ACCESS,
  selectMerchant,
  server,
} from "#orck/admin/test/harness"
import type { CatalogDraft } from "#orck/client/generated/wire"
import {
  act,
  browserEnvironment,
  click,
  mount,
  unmount,
} from "#orck/admin/test/mount"
import { CatalogCopilotPanel } from "./copilot-panel"

afterEach(unmount)

it("asks the console assistant and renders its answer", async () => {
  browserEnvironment()
  const requests = await server({
    "POST /console/catalog/ask": {
      answer: "Premium has a monthly price.",
      evidence: [],
      drafts: [],
    },
  })
  selectMerchant("merchant-one")
  await mount(
    <QueryClientProvider client={client()}>
      <CatalogCopilotPanel enabled draftingEnabled={false} />
    </QueryClientProvider>
  )
  const input = document.querySelector("input")!
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, "What do we sell?")
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
  await click("Ask")
  expect(document.body.textContent).toContain("Premium has a monthly price.")
  expect(requests.map((r) => `${r.method} ${r.path}`)).toEqual([
    "POST /console/catalog/ask",
  ])
  expect(requests[0].body).toEqual({ question: "What do we sell?" })
})

it("makes no request when the assistant capability is disabled", async () => {
  browserEnvironment()
  const requests = await server({})
  selectMerchant("merchant-one")
  await mount(
    <QueryClientProvider client={client()}>
      <CatalogCopilotPanel enabled={false} draftingEnabled={false} />
    </QueryClientProvider>
  )
  expect(document.querySelector("input")).toBeNull()
  expect(requests).toEqual([])
})

const price = aPrice("premium:monthly:v1", "prod_premium", { key: "monthly" })
const createPrice = {
  product_key: "premium",
  key: "monthly",
  unit_amount: "18000000",
  currency: "USD",
}
const drafts: CatalogDraft[] = [
  {
    kind: "new_price",
    price_change: null,
    refusal: null,
    new_price: {
      product_key: "premium",
      review_text: "A new monthly option.",
      create_price: createPrice,
    },
  },
  {
    kind: "price_change",
    new_price: null,
    refusal: null,
    price_change: {
      product_key: "premium",
      price_key: "monthly",
      current_amount: price.unit_amount,
      new_amount: createPrice.unit_amount,
      currency: "USD",
      affected_count: 3,
      review_text: "Lower the monthly price.",
      create_price: createPrice,
      migration: null,
    },
  },
]

async function showDrafts(catalog: "read" | "update", draftingEnabled = true) {
  browserEnvironment()
  const requests = await server({
    "GET /admin/access": { ...FULL_ACCESS, catalog },
    "POST /console/catalog/ask": {
      answer: "Review these options.",
      evidence: [],
      drafts,
    },
    "GET /catalog/products": {
      data: [aProduct("prod_premium", 0, { key: "premium", prices: [price] })],
      next_cursor: null,
    },
    "GET /merchant/configuration": { settings: {} },
    "POST /catalog/prices": aPrice("premium:monthly:v2", "prod_premium"),
  })
  selectMerchant("merchant-one")
  const queries = client()
  await mount(
    <QueryClientProvider client={queries}>
      <CatalogCopilotPanel enabled draftingEnabled={draftingEnabled} />
    </QueryClientProvider>
  )
  await act(async () => {
    const input = document.querySelector("input")!
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, "What could change?")
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })
  await click("Ask")
  return { requests, queries }
}

const buttonNamed = (name: string) =>
  [...document.querySelectorAll<HTMLButtonElement>("button")].find(
    (button) => button.textContent === name
  )
const writes = (requests: Awaited<ReturnType<typeof server>>) =>
  requests.filter(
    (request) =>
      request.method === "POST" && request.path !== "/console/catalog/ask"
  )

it.each([
  ["read", true],
  ["update", false],
] as const)(
  "keeps drafts read-only for %s access with drafting enabled=%s",
  async (catalog, enabled) => {
    const { requests, queries } = await showDrafts(catalog, enabled)
    expect(document.body.textContent).toContain("A new monthly option.")
    expect(document.body.textContent).toContain("Lower the monthly price.")
    expect(buttonNamed("Create this price")).toBeUndefined()
    expect(buttonNamed("Review in wizard")).toBeUndefined()
    expect(buttonNamed("Confirm")).toBeUndefined()
    expect(writes(requests)).toEqual([])
    expect(
      requests.some((request) => request.path === "/catalog/products")
    ).toBe(false)
    queries.clear()
  }
)

it("applies either draft only with catalog update authority", async () => {
  const { requests, queries } = await showDrafts("update")
  await click("Create this price")
  expect(writes(requests)).toHaveLength(1)
  expect(writes(requests)[0].body).toEqual(createPrice)
  await click("Review in wizard")
  await click("Confirm")
  expect(writes(requests).map((request) => request.path)).toEqual([
    "/catalog/prices",
    "/catalog/prices",
  ])
  expect(writes(requests)[1].body).toMatchObject({
    product_id: "prod_premium",
    key: "monthly",
    unit_amount: "18000000",
    expected_revision: 1,
  })
  queries.clear()
})

it("removes draft actions and an open wizard when update authority is lost", async () => {
  const { requests, queries } = await showDrafts("update")
  await click("Review in wizard")
  expect(buttonNamed("Confirm")).toBeDefined()
  await act(async () => {
    queries.setQueryData(adminQueries.access().queryKey, {
      ...FULL_ACCESS,
      catalog: "read",
    })
  })
  expect(buttonNamed("Create this price")).toBeUndefined()
  expect(buttonNamed("Review in wizard")).toBeUndefined()
  expect(buttonNamed("Confirm")).toBeUndefined()
  expect(document.body.textContent).toContain("Lower the monthly price.")
  expect(writes(requests)).toEqual([])
  queries.clear()
})
