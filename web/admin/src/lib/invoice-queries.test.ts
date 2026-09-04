import { beforeEach, describe, expect, it, vi } from "vitest"
import { QueryClient } from "@tanstack/react-query"
import { applyInvoiceAction, listInvoices } from "./api/invoice-endpoints"
import {
  invoiceActionMutation,
  invoiceKeys,
  invoiceProfileMutation,
  invoiceQueries,
} from "./invoice-queries"
import type { InvoiceProfile } from "./api/invoice-types"

const mocks = vi.hoisted(() => ({ api: vi.fn(), merchant: "merchant-a" }))
vi.mock("./api/client", () => ({
  api: mocks.api,
  getTokens: () => ({ merchant: mocks.merchant }),
}))
beforeEach(() => {
  vi.clearAllMocks()
  mocks.merchant = "merchant-a"
})
describe("invoice request and cache contract", () => {
  it("keeps the same collection key and method when a request is retried", async () => {
    const request = {
      id: "invoice-1",
      action: "retry_collection" as const,
      paymentMethodId: "method-1",
      idempotencyKey: "operation-1",
    }
    await applyInvoiceAction(request)
    await applyInvoiceAction(request)
    expect(mocks.api).toHaveBeenNthCalledWith(
      1,
      "/merchant/invoices/invoice-1/retry-collection",
      {
        method: "POST",
        headers: { "Idempotency-Key": "operation-1" },
        body: { payment_method_id: "method-1" },
      }
    )
    expect(mocks.api.mock.calls[1]).toEqual(mocks.api.mock.calls[0])
  })
  it("forwards filters and server pagination without post-filtering", async () => {
    const signal = new AbortController().signal
    await listInvoices({ currency: "JPY", status: "past_due" }, 25, 50, signal)
    expect(mocks.api).toHaveBeenCalledWith("/merchant/invoices", {
      query: { currency: "JPY", status: "past_due", limit: 25, offset: 50 },
      signal,
    })
    expect(invoiceQueries.list({}, 25, 0).queryKey).not.toEqual(
      invoiceQueries.list({}, 25, 25).queryKey
    )
  })
  it("invalidates the original merchant after a mutation, including errors", async () => {
    const client = new QueryClient(),
      invalidate = vi.spyOn(client, "invalidateQueries").mockResolvedValue()
    const options = invoiceActionMutation(client, "customer-1"),
      root = invoiceKeys.root()
    mocks.merchant = "merchant-b"
    mocks.api.mockRejectedValueOnce(new Error("uncertain"))
    const mutation = client.getMutationCache().build(client, options)
    await expect(
      mutation.execute({ id: "invoice-1", action: "void" })
    ).rejects.toThrow("uncertain")
    expect(invalidate).toHaveBeenCalledWith({ queryKey: root })
    expect(
      invalidate.mock.calls.every(
        ([filters]) => filters?.queryKey?.[1] === "merchant-a"
      )
    ).toBe(true)
  })
  it("profile changes refresh the profile and do not overwrite cached issued invoices", async () => {
    const client = new QueryClient()
    const invoiceKey = invoiceKeys.detail("invoice-1")
    client.setQueryData(invoiceKey, { po_number: "OLD" })
    const mutation = client
      .getMutationCache()
      .build(client, invoiceProfileMutation(client, "customer-1"))
    await mutation.execute({
      net_terms_days: 7,
      collection_method: "send_invoice",
      po_number: "NEW",
    } as InvoiceProfile)
    expect(client.getQueryData(invoiceKey)).toEqual({ po_number: "OLD" })
  })
})
