// @vitest-environment jsdom
// Granting product access mounted against the real API client: a preset is
// hours added after any access the customer has, a date ends the grant at
// that UTC day's close as an instant, and "never" sends no term.
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter } from "react-router-dom"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  aProduct,
  client,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"

import { GrantProductAccessDialog } from "./detail"
import { GRANT_DURATIONS, MAX_GRANT_HOURS, grantTerm } from "./grant-duration"

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {
    "/admin/catalog/products": {
      data: [aProduct("prod_gift", 0, { display_name: "Gift" })],
      next_cursor: null,
    },
    "POST /admin/product-access": { items: [] },
  }
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

const pick = async (label: string, option: string) => {
  await act(async () =>
    document.querySelector<HTMLElement>(`[id="${label}"]`)!.click()
  )
  const found = [
    ...document.querySelectorAll<HTMLElement>('[role="option"]'),
  ].find((node) => node.textContent?.startsWith(option))
  expect(found, option).toBeDefined()
  await act(async () => found!.click())
}

const grant = async (duration: string) => {
  await mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter>
        <GrantProductAccessDialog customerId="cus_1" />
      </MemoryRouter>
    </QueryClientProvider>
  )
  const trigger = [...document.querySelectorAll("button")].find(
    (b) => b.textContent?.trim() === "Product access"
  )!
  await act(async () => trigger.click())
  await vi.waitFor(() =>
    expect(document.querySelector("#pa-product")).not.toBeNull()
  )
  await pick("pa-product", "Gift")
  await pick("pa-duration", duration)
}

const granted = () =>
  (
    requests.find((r) => r.method === "POST")?.body as {
      items: Record<string, unknown>[]
    }
  )?.items[0]

describe("grant durations", () => {
  it("sends a preset as hours", async () => {
    await grant("7 days")
    await click("Grant access")
    await vi.waitFor(() => expect(granted()).toBeDefined())
    expect(granted()).toEqual({
      customer_id: "cus_1",
      product_id: "prod_gift",
      hours: 168,
    })
  })

  it("sends never as no term", async () => {
    await grant("Never expires")
    await click("Grant access")
    await vi.waitFor(() => expect(granted()).toBeDefined())
    expect(granted()).toEqual({ customer_id: "cus_1", product_id: "prod_gift" })
  })

  it("ends a dated grant at the close of that UTC day", () => {
    const now = new Date("2026-10-11T12:00:00Z")
    expect(grantTerm("until", "2026-10-31", now)).toEqual({
      endsAt: "2026-11-01T00:00:00.000Z",
    })
    expect(grantTerm("until", "2026-10-11", now)).toEqual({
      endsAt: "2026-10-12T00:00:00.000Z",
    })
    expect(grantTerm("until", "2026-10-10", now)).toBeNull()
    expect(grantTerm("until", "", now)).toBeNull()
  })

  it("keeps every preset within the API's longest grant", () => {
    const hours = GRANT_DURATIONS.flatMap((d) =>
      "days" in d ? [d.days * 24] : []
    )
    expect(hours).toEqual([24, 168, 720, 8760])
    expect(Math.max(...hours)).toBeLessThanOrEqual(MAX_GRANT_HOURS)
    // math.MaxInt64 / int64(time.Hour), as the API computes it.
    expect(MAX_GRANT_HOURS).toBe(Number((2n ** 63n - 1n) / 3_600_000_000_000n))
  })
})
