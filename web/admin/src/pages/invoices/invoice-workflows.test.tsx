// @vitest-environment jsdom
// Invoice support mounted against the real API client: collection retries
// pick from the customer's saved cards, payment history walks its cursors,
// and a customer without an invoice profile gets one on the first save.
import { QueryClientProvider } from "@tanstack/react-query"
import type { ReactNode } from "react"
import { MemoryRouter } from "react-router-dom"
import { afterEach, beforeEach, expect, it, vi } from "vitest"

import type { InvoicePayment } from "@/lib/api/generated/wire"
import {
  aPaymentMethod,
  anInvoice,
  client,
  cursorPages,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { act, browserEnvironment, click, mount, unmount } from "@/test/mount"
import { CustomerInvoiceProfileSection } from "../customers/invoice-profile"
import { InvoiceDetail } from "./detail"

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
const invoice = anInvoice("inv_1", {
  amount_due: "5000000",
  available_actions: ["retry_collection"],
})
const paid = (i: number): InvoicePayment => ({
  id: `invpay_${i}`,
  invoice_id: "inv_1",
  currency: "USD",
  amount: "1000000",
  status: "failed",
  payment_method_id: "pm_1",
  rail: "nmi",
  transaction_id: `txn_${i}`,
  failure_code: "05",
  failure_reason: "do not honor",
  attempted_at: "2026-09-18T00:00:00Z",
  settled_at: null,
})

it("retries collection with a card read from the customer's saved methods", async () => {
  routes["/admin/customers/cus_1/payment-methods"] = cursorPages(
    [aPaymentMethod("pm_1")],
    100
  )
  routes["POST /admin/invoices/inv_1/retry-collection"] = {
    invoice: { ...invoice, status: "paid" },
    payment: { ...paid(9), status: "settled" },
    replayed: false,
  }
  await show(<InvoiceDetail invoice={invoice} />)
  expect(sent("/admin/customers/cus_1/payment-methods")).toHaveLength(0)
  await click("Retry collection")
  await vi.waitFor(() => expect(text()).toContain("visa ••••4242 (nmi)"))
  const select = document.querySelector<HTMLSelectElement>(
    "#invoice-payment-method"
  )!
  await act(async () => {
    select.value = "pm_1"
    select.dispatchEvent(new Event("change", { bubbles: true }))
  })
  await click("Confirm")
  const [retry] = sent("/admin/invoices/inv_1/retry-collection")
  expect(retry.body).toEqual({ payment_method_id: "pm_1" })
  expect(retry.headers.get("Idempotency-Key")).toMatch(/^[\da-f-]{36}$/)
})

it("pages the payment history by cursor", async () => {
  routes["/admin/invoices/inv_1/payments"] = cursorPages(
    Array.from({ length: 21 }, (_, i) => paid(i)),
    20
  )
  await show(<InvoiceDetail invoice={invoice} />)
  await vi.waitFor(() => expect(text()).toContain("txn_19"))
  const next = document.querySelector<HTMLButtonElement>(
    'button[aria-label="Next page"]'
  )!
  await act(async () => next.click())
  await vi.waitFor(() => expect(text()).toContain("txn_20"))
  expect(sent("/admin/invoices/inv_1/payments").map((r) => r.query)).toEqual(
    ["limit=20", "limit=20&cursor=20"]
  )
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
