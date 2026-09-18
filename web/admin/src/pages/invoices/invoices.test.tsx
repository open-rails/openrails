// Invoice support: the requests the console makes, the authority it takes
// from the server, and the amounts it shows.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { InvoiceProfile, InvoiceRetryResponse, MerchantInvoice } from "@/lib/api/invoice-types"
import {
  invoiceActionMutation, invoiceKeys, invoiceProfileMutation, invoiceQueries,
} from "@/lib/invoice-queries"
import { queryKeys } from "@/lib/queries"
import {
  calls, client, exec, MAX_INT64, render, selectMerchant, server,
  type Recorded, type Reply,
} from "@/test/harness"
import { InvoiceProfileEditor } from "../customers/invoice-profile"
import { InvoiceDetail } from "./detail"
import {
  allowedInvoiceActions, invoiceProfileRequest, invoiceProfileValues, invoiceResultMessage,
} from "./model"

const invoice = (
  actions: MerchantInvoice["available_actions"],
  overrides: Partial<MerchantInvoice> = {}
): MerchantInvoice => ({
  id: "invoice-1", customer_id: "customer-1", currency: "JPY", unit_decimals: 4,
  invoice_number: "INV-1", status: "open", period_from: "2026-09-01T00:00:00Z",
  period_to: "2026-10-01T00:00:00Z", total_amount: "120000", subtotal_amount: "120000",
  amount_paid: "20000", amount_due: "100000", collection_method: "send_invoice",
  collection_failure_count: 0, available_actions: actions,
  line_items: [{ event_type: "usage", amount: "120000", count: 1 }],
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
    await queries.fetchQuery(invoiceQueries.list({ currency: "JPY", status: "past_due" }, 25, 50))
    expect(requests[0].query).toBe("currency=JPY&status=past_due&limit=25&offset=50")
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
    await exec(queries, invoiceProfileMutation(queries, "customer-1"), {
      net_terms_days: 7, collection_method: "send_invoice", po_number: "NEW",
    } as InvoiceProfile)
    expect(calls(requests)).toEqual(["PUT /merchant/customers/customer-1/invoice-profile"])
    expect(queries.getQueryData(invoiceKey)).toEqual({ po_number: "OLD" })
  })
})

describe("invoice support model", () => {
  it("preserves profile tax facts and validates terms and contacts", () => {
    const original: InvoiceProfile = {
      net_terms_days: 30, collection_method: "send_invoice", po_number: " PO-1 ",
      billing_contacts: [{ email: "ap@example.test" }],
      tax: { tax_id: "VAT-1", registration: { country: "GB" }, rate: 0.2 },
    }
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
      allowedInvoiceActions({ available_actions: available } as unknown as MerchantInvoice)
    expect(actions([])).toEqual([])
    expect(actions(["void"])).toEqual(["void"])
  })

  it("never describes a failed or uncertain collection as paid", () => {
    const result = (status: string, replayed = false) =>
      ({ attempt: { status }, replayed }) as unknown as InvoiceRetryResponse
    expect(invoiceResultMessage(result("failed"))).toContain("failed")
    expect(invoiceResultMessage(result("attempted"))).toContain("pending verification")
    expect(invoiceResultMessage(result("settled", true))).toBe("Existing payment confirmed.")
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
          currency: "USD", unit_decimals: 6,
          total_amount: MAX_INT64, subtotal_amount: MAX_INT64,
          amount_paid: "9007199254740993", amount_due: "9214364837600034814",
          line_items: [{ event_type: "usage", amount: MAX_INT64, count: 1 }],
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

  it("makes a read-only profile inspectable without a save control", () => {
    const html = render(
      <InvoiceProfileEditor
        customerId="customer-1"
        canUpdate={false}
        profile={{ net_terms_days: 30, collection_method: "send_invoice", po_number: "PO-1" }}
      />
    )
    expect(html).toContain("PO-1")
    expect(html).toContain("disabled")
    expect(html).not.toContain("Save invoice profile")
  })
})
