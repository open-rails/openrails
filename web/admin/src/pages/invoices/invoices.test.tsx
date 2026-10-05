// Invoice support: the requests the console makes, the authority it takes
// from the server, and the amounts it shows.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { Invoice, InvoiceCollection, InvoiceProfile } from "@/lib/api/generated/wire"
import {
  invoiceActionMutation, invoiceKeys, invoiceProfileMutation, invoiceQueries,
} from "@/lib/invoice-queries"
import { queryKeys } from "@/lib/queries"
import {
  anInvoice, calls, client, exec, MAX_INT64, render, selectMerchant, server,
  type Recorded, type Reply,
} from "@/test/harness"
import { InvoiceProfileEditor } from "../customers/invoice-profile"
import { InvoiceDetail } from "./detail"
import {
  allowedInvoiceActions, invoiceProfileRequest, invoiceProfileValues, invoiceResultMessage,
} from "./model"

const invoice = (actions: Invoice["available_actions"], overrides: Partial<Invoice> = {}) =>
  anInvoice("invoice-1", {
    customer_id: "customer-1", currency: "JPY", invoice_number: "INV-1",
    total_amount: "120000", subtotal_amount: "120000", amount_paid: "20000", amount_due: "100000",
    collection_method: "send_invoice", available_actions: actions,
    line_items: [{ event_type: "usage", amount: "120000", count: 1, dimensions: null }],
    ...overrides,
  })
const profile = (overrides: Partial<InvoiceProfile> = {}): InvoiceProfile => ({
  net_terms_days: 30, collection_method: "send_invoice", po_number: "", tax: null,
  billing_contacts: [], memo: "", ...overrides,
})

let requests: Recorded[]
let routes: Record<string, Reply>
beforeEach(async () => {
  routes = {}
  requests = await server(routes)
  selectMerchant("merchant-a")
})
afterEach(() => vi.unstubAllGlobals())

describe("invoice requests and cache", () => {
  it("replays a retry under the same operation identity", async () => {
    const queries = client()
    const retry = {
      id: "invoice-1", action: "retry_collection" as const,
      paymentMethodId: "method-1", idempotencyKey: "operation-1",
    }
    const options = invoiceActionMutation(queries, "customer-1")
    await exec(queries, options, retry)
    await exec(queries, options, retry)
    expect(calls(requests)).toEqual([
      "POST /merchant/invoices/invoice-1/retry-collection",
      "POST /merchant/invoices/invoice-1/retry-collection",
    ])
    for (const attempt of requests) {
      expect(attempt.headers.get("Idempotency-Key")).toBe("operation-1")
      expect(attempt.body).toEqual({ payment_method_id: "method-1" })
    }
  })

  it("leaves filtering and paging to the server", async () => {
    const queries = client()
    await queries.fetchQuery(invoiceQueries.list({ currency: "JPY", status: "past_due" }, 25, "cur_2"))
    await queries.fetchQuery(invoiceQueries.payments("invoice-1", 20, "cur_3"))
    expect(requests.map((r) => r.query)).toEqual([
      "currency=JPY&status=past_due&limit=25&cursor=cur_2", "limit=20&cursor=cur_3",
    ])
  })

  it("reads a customer without a profile as none, and any other failure as one", async () => {
    const queries = client()
    routes["/merchant/customers/customer-1/invoice-profile"] = () =>
      Response.json({ error: { code: "resource_not_found", message: "no profile" } }, { status: 404 })
    expect(await queries.fetchQuery(invoiceQueries.profile("customer-1"))).toBeNull()
    routes["/merchant/customers/customer-2/invoice-profile"] = () =>
      Response.json({ error: { code: "service_unavailable", message: "down" } }, { status: 503 })
    await expect(queries.fetchQuery(invoiceQueries.profile("customer-2"))).rejects.toThrow("down")
  })

  it("refreshes the merchant that started an action, even when it fails", async () => {
    const queries = client()
    const options = invoiceActionMutation(queries, "customer-1")
    const root = invoiceKeys.root()
    queries.setQueryData(root, {})
    queries.setQueryData(queryKeys.customer("customer-1"), {})
    routes["POST /merchant/invoices/invoice-1/void"] = () =>
      Response.json({ error: { message: "uncertain" } }, { status: 503 })
    selectMerchant("merchant-b")
    await expect(exec(queries, options, { id: "invoice-1", action: "void" })).rejects.toThrow("uncertain")
    expect(queries.getQueryState(root)?.isInvalidated).toBe(true)
    expect(root[1]).toBe("merchant-a")
  })

  it("refreshes a saved profile without overwriting the issued invoice", async () => {
    const queries = client()
    const invoiceKey = invoiceKeys.detail("invoice-1")
    queries.setQueryData(invoiceKey, { po_number: "OLD" })
    await exec(queries, invoiceProfileMutation(queries, "customer-1"), profile({ net_terms_days: 7, po_number: "NEW" }))
    expect(calls(requests)).toEqual(["PUT /merchant/customers/customer-1/invoice-profile"])
    expect(queries.getQueryData(invoiceKey)).toEqual({ po_number: "OLD" })
  })
})

describe("invoice support model", () => {
  it("preserves profile tax facts and validates terms and contacts", () => {
    const original = profile({
      po_number: " PO-1 ", billing_contacts: [{ name: "", email: "ap@example.test" }],
      tax: { tax_id: "VAT-1", registration: { country: "GB" }, rate: 0.2 },
    })
    const values = invoiceProfileValues(original)
    expect(invoiceProfileRequest(values, original)).toMatchObject({
      net_terms_days: 30, po_number: "PO-1", tax: original.tax,
    })
    for (const invalid of [
      { ...values, terms: "-1" },
      { ...values, contacts: [{ name: "AP", email: "bad" }] },
      { ...values, tax: [{ key: "tax_id", value: "a" }, { key: "tax_id", value: "b" }] },
    ])
      expect(() => invoiceProfileRequest(invalid, original)).toThrow()
  })

  it("takes the offered actions from the server, not from role names", () => {
    const actions = (available: string[]) =>
      allowedInvoiceActions({ available_actions: available } as unknown as Invoice)
    expect(actions([])).toEqual([])
    expect(allowedInvoiceActions(invoice([]))).toEqual([])
    expect(actions(["void"])).toEqual(["void"])
  })

  it("never describes a failed or uncertain collection as paid", () => {
    const result = (status: string, replayed = false) =>
      ({ payment: { status }, replayed }) as unknown as InvoiceCollection
    expect(invoiceResultMessage(result("failed"))).toContain("failed")
    expect(invoiceResultMessage(result("attempted"))).toContain("pending verification")
    expect(invoiceResultMessage(result("settled", true))).toBe("Existing payment confirmed.")
    expect(invoiceResultMessage(invoice([]))).toBe("Invoice updated.")
  })
})

describe("invoice rendering", () => {
  it("shows currency-scaled amounts and the records they belong to", () => {
    const html = render(<InvoiceDetail invoice={invoice([])} />)
    expect(html).toContain('href="/invoices"')
    expect(html).toContain('href="/customers/customer-1"')
    expect(html).toContain("¥12")
    expect(html).toContain("¥10")
    expect(html).not.toContain("¥0.12")
    expect(html).not.toContain("Void invoice")
  })

  it("renders int64 amounts exactly and only the offered actions", () => {
    const html = render(
      <InvoiceDetail
        invoice={invoice(["void", "record_payment"], {
          currency: "USD",
          total_amount: MAX_INT64, subtotal_amount: MAX_INT64,
          amount_paid: "9007199254740993", amount_due: "9214364837600034814",
          line_items: [{ event_type: "usage", amount: MAX_INT64, count: 1, dimensions: null }],
        })}
      />
    )
    expect(html).toContain("$9,223,372,036,854.775807")
    expect(html).toContain("$9,007,199,254.740993")
    expect(html).toContain("$9,214,364,837,600.034814")
    expect(html).not.toContain("exceeds the exact display range")
    expect(html).toContain("Void invoice")
    expect(html).toContain("Record payment")
    expect(html).not.toContain("Retry collection")
  })

  it("shows an unresolved collection operation instead of offering actions", () => {
    const html = render(
      <InvoiceDetail invoice={invoice([], { recovery: { retryable: false, blocked_reason: "payment_in_progress", operation: { id: "op_1", status: "pending" } } })} />
    )
    expect(html).toContain("Collection operation op_1 (pending)")
    expect(html).not.toContain("No actions are available")
  })

  it("edits a stored profile, and an empty one when the customer has none", () => {
    const stored = render(<InvoiceProfileEditor customerId="customer-1" profile={profile({ po_number: "PO-1" })} />)
    expect(stored).toContain("PO-1")
    expect(stored).toContain("Save invoice profile")
    const empty = render(<InvoiceProfileEditor customerId="customer-1" profile={null} />)
    expect(empty).toContain('value="0"')
    expect(empty).toContain("Save invoice profile")
  })
})
