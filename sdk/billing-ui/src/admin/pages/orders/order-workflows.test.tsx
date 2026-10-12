import { AdminTestProvider as QueryClientProvider } from "#orck/admin/test/harness"
import { MemoryRouter } from "#orck/admin/test/router"
// @vitest-environment jsdom
// Order support mounted against the real API client: staff cannot initiate card charges, payment history walks its cursors,
// and a customer without an invoice profile gets one on the first save.

import type { ReactNode } from "react"

import { afterEach, beforeEach, expect, it, vi } from "vitest"

import {
  aPayment,
  aUsageOrder,
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
import { CustomerInvoiceProfileSection } from "../customers/invoice-profile"
import { OrderDetail } from "./detail"

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  browserEnvironment()
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(unmount)

const show = (node: ReactNode) =>
  mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter>{node}</MemoryRouter>
    </QueryClientProvider>
  )
const text = () => document.body.textContent ?? ""
const sent = (path: string) => requests.filter((r) => r.path === path)
const base = aUsageOrder("ord_1")
const order = {
  ...base,
  total: "5000000",
  usage: { ...base.usage!, amount_due: "5000000" },
}
const paid = (i: number) =>
  aPayment(`pay_${i}`, {
    order_id: "ord_1",
    price_id: null,
    amount: "1000000",
    transaction_id: `txn_${i}`,
  })

it("does not expose immediate saved-card collection", async () => {
  await show(<OrderDetail order={order} />)
  expect(text()).not.toContain("Retry collection")
  expect(document.querySelector("#order-payment-method")).toBeNull()
  expect(sent("/admin/customers/cus_1/payment-methods")).toHaveLength(0)
})

it.each(["provider", "nmi_schedule"] as const)("offers history but no manual settlement for %s bills", async (collection_owner) => {
  await show(<OrderDetail order={{ ...order, collection_owner }} />)
  expect(text()).not.toContain("Record payment")
  expect(text()).not.toContain("Waive balance")
  expect(text()).not.toContain("Mark uncollectible")
  expect(text()).toContain("Payment history")
  expect(requests.some((request) => request.method === "POST")).toBe(false)
})

it("pages the payment history by cursor", async () => {
  routes["/admin/payments"] = cursorPages(
    Array.from({ length: 26 }, (_, i) => paid(i)),
    25
  )
  await show(<OrderDetail order={order} />)
  await vi.waitFor(() => expect(text()).toContain("txn_24"))
  expect(text()).toContain("Showing 1–25")
  const next = document.querySelector<HTMLButtonElement>(
    'button[aria-label="Next page"]'
  )!
  await act(async () => next.click())
  await vi.waitFor(() => expect(text()).toContain("txn_25"))
  expect(text()).toContain("Showing 26–26")
  expect(sent("/admin/payments").map((r) => r.query)).toEqual([
    "order_id=ord_1&limit=25",
    "order_id=ord_1&limit=25&cursor=25",
  ])
})

it("creates the profile of a customer who has none", async () => {
  routes["GET /admin/customers/cus_1"] = {
    id: "cus_1",
    settings: {
      credit_limits: [],
      trust_levels: [],
      billing_policy: null,
      invoice_profile: null,
    },
  }
  routes["PATCH /admin/customers/cus_1"] = (request) => ({
    id: "cus_1",
    settings: request.body,
  })
  await show(<CustomerInvoiceProfileSection customerId="cus_1" />)
  await vi.waitFor(() => expect(text()).toContain("Save invoice profile"))
  await click("Save invoice profile")
  await vi.waitFor(() => expect(text()).toContain("Invoice profile saved"))
  expect(requests.find((r) => r.method === "PATCH")!.body).toEqual({
    invoice_profile: {
      net_terms_days: 0,
      collection_method: "charge_automatically",
      po_number: "",
      memo: "",
      billing_contacts: [],
      tax: {},
    },
  })
})
