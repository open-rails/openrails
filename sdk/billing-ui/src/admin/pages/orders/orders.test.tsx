import { invoiceProfileMutation } from "#orck/admin/test/harness"
import { orderKeys } from "#orck/admin/test/harness"
import { orderQueries } from "#orck/admin/test/harness"
import { usageActionMutation } from "#orck/admin/test/harness"
import { queryKeys } from "#orck/admin/test/harness"
// Order support: the requests the console makes, the actions a usage order's
// state allows, and the amounts it shows.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type {
  InvoiceProfile,
  Order,
  Payment,
} from "#orck/client/generated/wire"

import {
  aUsageOrder,
  calls,
  client,
  exec,
  MAX_INT64,
  render,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "#orck/admin/test/harness"
import { InvoiceProfileEditor } from "../customers/invoice-profile"
import {
  invoiceProfileRequest,
  invoiceProfileValues,
} from "../customers/invoice-profile-model"
import { OrderDetail } from "./detail"
import { usageActions, usageResultMessage } from "./model"

const order = (
  overrides: Partial<Order> = {},
  usage: Partial<NonNullable<Order["usage"]>> = {}
) => {
  const base = aUsageOrder("order-1", {
    customer_id: "customer-1",
    currency: "JPY",
    number: null,
    total: "120000",
    lines: [
      {
        id: "line-1",
        price_id: null,
        product_id: null,
        source: "metered:tokens",
        description: "Usage: metered:tokens",
        quantity: null,
        unit_amount: "120000",
        amount: "120000",
        ownership: null,
        billing_interval_hours: null,
        access_duration_hours: null,
        subscription_id: null,
        product_access_id: null,
      },
    ],
    ...overrides,
  })
  return {
    ...base,
    usage: {
      ...base.usage!,
      amount_paid: "20000",
      amount_due: "100000",
      collection_method: "send_invoice" as const,
      ...usage,
    },
  }
}
const profile = (overrides: Partial<InvoiceProfile> = {}): InvoiceProfile => ({
  net_terms_days: 30,
  collection_method: "send_invoice",
  po_number: "",
  tax: null,
  billing_contacts: [],
  memo: "",
  ...overrides,
})

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(() => vi.unstubAllGlobals())

describe("order requests and cache", () => {
  it("posts each usage action to its route", async () => {
    const queries = client()
    const options = usageActionMutation(queries, "customer-1")
    await exec(queries, options, { id: "order-1", action: "waive_balance" })
    await exec(queries, options, {
      id: "order-1",
      action: "mark_uncollectible",
    })
    await exec(queries, options, {
      id: "order-1",
      action: "record_payment",
      amount: "5",
      reference: "wire-1",
    })
    expect(calls(requests)).toEqual([
      "POST /admin/orders/order-1/waive-balance",
      "POST /admin/orders/order-1/mark-uncollectible",
      "POST /admin/payments",
    ])
    expect(requests[2].body).toEqual({
      order_id: "order-1",
      amount: "5",
      transaction_id: "wire-1",
    })
  })

  it("leaves filtering and paging to the server", async () => {
    const queries = client()
    await queries.fetchQuery(
      orderQueries.list(
        { origin: "usage", status: "open", overdue: "true" },
        25,
        "cur_2"
      )
    )
    await queries.fetchQuery(orderQueries.payments("order-1", 20, "cur_3"))
    expect(requests.map((r) => r.query)).toEqual([
      "origin=usage&status=open&overdue=true&limit=25&cursor=cur_2",
      "order_id=order-1&limit=20&cursor=cur_3",
    ])
  })

  it("reads a customer without a profile as none, and any other failure as one", async () => {
    const queries = client()
    routes["GET /admin/customers/customer-1"] = () => ({
      id: "customer-1",
      settings: {
        credit_limits: [],
        trust_levels: [],
        billing_policy: null,
        invoice_profile: null,
      },
    })
    expect(
      await queries.fetchQuery(orderQueries.profile("customer-1"))
    ).toBeNull()
    expect(calls(requests).at(-1)).toBe("GET /admin/customers/customer-1")
    routes["GET /admin/customers/customer-2"] = () =>
      Response.json(
        { error: { code: "service_unavailable", message: "down" } },
        { status: 503 }
      )
    await expect(
      queries.fetchQuery(orderQueries.profile("customer-2"))
    ).rejects.toThrow("down")
  })

  it("refreshes the merchant that started an action, even when it fails", async () => {
    const queries = client()
    const options = usageActionMutation(queries, "customer-1")
    const root = orderKeys.root()
    queries.setQueryData(root, {})
    queries.setQueryData(queryKeys.customer("customer-1"), {})
    routes["POST /admin/orders/order-1/waive-balance"] = () =>
      Response.json({ error: { message: "uncertain" } }, { status: 503 })
    selectMerchant("merchant-b")
    await expect(
      exec(queries, options, { id: "order-1", action: "waive_balance" })
    ).rejects.toThrow("uncertain")
    expect(queries.getQueryState(root)?.isInvalidated).toBe(true)
    expect(root[1]).toBe("merchant-a")
  })

  it("refreshes a saved profile without overwriting a billed order", async () => {
    const queries = client()
    const orderKey = orderKeys.detail("order-1")
    queries.setQueryData(orderKey, { po_number: "OLD" })
    routes["PATCH /admin/customers/customer-1"] = (request) => ({
      id: "customer-1",
      settings: request.body,
    })
    await exec(
      queries,
      invoiceProfileMutation(queries, "customer-1"),
      profile({ net_terms_days: 7, po_number: "NEW" })
    )
    expect(calls(requests)).toEqual(["PATCH /admin/customers/customer-1"])
    expect(queries.getQueryData(orderKey)).toEqual({ po_number: "OLD" })
  })
})

describe("order support model", () => {
  it("preserves profile tax facts and validates terms and contacts", () => {
    const original = profile({
      po_number: " PO-1 ",
      billing_contacts: [{ name: "", email: "ap@example.test" }],
      tax: { tax_id: "VAT-1", registration: { country: "GB" }, rate: 0.2 },
    })
    const values = invoiceProfileValues(original)
    expect(invoiceProfileRequest(values, original)).toMatchObject({
      net_terms_days: 30,
      po_number: "PO-1",
      tax: original.tax,
    })
    for (const invalid of [
      { ...values, terms: "-1" },
      { ...values, contacts: [{ name: "AP", email: "bad" }] },
      {
        ...values,
        tax: [
          { key: "tax_id", value: "a" },
          { key: "tax_id", value: "b" },
        ],
      },
    ])
      expect(() => invoiceProfileRequest(invalid, original)).toThrow()
  })

  it("offers the actions a usage order's state allows", () => {
    expect(usageActions(order())).toEqual([
      "record_payment",
      "mark_uncollectible",
      "waive_balance",
    ])
    expect(
      usageActions(order({}, { collection_method: "charge_automatically" }))
    ).toEqual(["record_payment", "mark_uncollectible", "waive_balance"])
    expect(usageActions(order({ status: "uncollectible" }))).toEqual([
      "record_payment",
      "waive_balance",
    ])
    for (const status of ["processing", "complete", "void"] as const)
      expect(usageActions(order({ status }))).toEqual([])
    expect(usageActions(order({}, { amount_due: "0" }))).toEqual([])
    for (const collection_owner of ["provider", "nmi_schedule"] as const)
      expect(usageActions(order({ collection_owner }))).toEqual([])
    expect(
      usageActions({ ...order(), origin: "customer", usage: null })
    ).toEqual([])
  })

  it("never describes an unresolved collection as paid", () => {
    expect(usageResultMessage(order({ status: "processing" }))).toContain(
      "pending verification"
    )
    expect(usageResultMessage(order({ status: "complete" }))).toBe(
      "Order paid."
    )
    expect(usageResultMessage({ kind: "charge" } as Payment)).toBe(
      "Payment recorded."
    )
    expect(usageResultMessage(order())).toBe("Order updated.")
  })
})

describe("order rendering", () => {
  it("shows currency-scaled amounts and the records they belong to", () => {
    const html = render(<OrderDetail order={order()} />)
    expect(html).toContain('href="/orders"')
    expect(html).toContain('href="/customers/customer-1"')
    expect(html).toContain("¥12")
    expect(html).toContain("¥10")
    expect(html).not.toContain("¥0.12")
    expect(html).toContain("metered:tokens")
  })

  it("renders int64 amounts exactly", () => {
    const html = render(
      <OrderDetail
        order={order(
          { currency: "USD", total: MAX_INT64 },
          { amount_paid: "9007199254740993", amount_due: "9214364837600034814" }
        )}
      />
    )
    expect(html).toContain("$9,223,372,036,854.775807")
    expect(html).toContain("$9,007,199,254.740993")
    expect(html).toContain("$9,214,364,837,600.034814")
    expect(html).not.toContain("exceeds the exact display range")
  })

  it("shows an in-progress collection instead of offering actions", () => {
    const html = render(<OrderDetail order={order({ status: "processing" })} />)
    expect(html).toContain("A collection of this order is in progress")
    expect(html).not.toContain("No actions are available")
  })

  it("edits a stored profile, and an empty one when the customer has none", () => {
    const stored = render(
      <InvoiceProfileEditor
        customerId="customer-1"
        profile={profile({ po_number: "PO-1" })}
      />
    )
    expect(stored).toContain("PO-1")
    expect(stored).toContain("Save invoice profile")
    const empty = render(
      <InvoiceProfileEditor customerId="customer-1" profile={null} />
    )
    expect(empty).toContain('value="0"')
    expect(empty).toContain("Save invoice profile")
  })
})
