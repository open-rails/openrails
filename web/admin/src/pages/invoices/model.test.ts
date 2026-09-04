import { describe, it, expect } from "vitest"
import {
  invoicePaymentAmount,
  invoiceProfileRequest,
  invoiceProfileValues,
  invoiceResultMessage,
  allowedInvoiceActions,
} from "./model"
import type {
  InvoiceProfile,
  MerchantInvoice,
  InvoiceRetryResponse,
} from "@/lib/api/invoice-types"

describe("invoice support models", () => {
  it("uses the invoice's native scale for remittance", () => {
    expect(invoicePaymentAmount("12", 120000, 4)).toBe(120000)
    expect(invoicePaymentAmount("2", 120000, 4)).toBe(20000)
    expect(invoicePaymentAmount("12", 12000000, 6)).toBe(12000000)
    expect(() => invoicePaymentAmount("0.00001", 120000, 4)).toThrow()
    expect(() => invoicePaymentAmount("13", 120000, 4)).toThrow()
    expect(() => invoicePaymentAmount("0", 120000, 4)).toThrow()
    expect(() =>
      invoicePaymentAmount("9007199254740993", Number.MAX_SAFE_INTEGER, 6)
    ).toThrow()
  })
  it("preserves profile tax facts and validates terms and contacts", () => {
    const original: InvoiceProfile = {
      net_terms_days: 30,
      collection_method: "send_invoice",
      po_number: " PO-1 ",
      billing_contacts: [{ email: "ap@example.test" }],
      tax: { tax_id: "VAT-1", registration: { country: "GB" }, rate: 0.2 },
    }
    const values = invoiceProfileValues(original)
    expect(invoiceProfileRequest(values, original)).toMatchObject({
      net_terms_days: 30,
      po_number: "PO-1",
      tax: original.tax,
    })
    expect(() =>
      invoiceProfileRequest({ ...values, terms: "-1" }, original)
    ).toThrow()
    expect(() =>
      invoiceProfileRequest(
        { ...values, contacts: [{ name: "AP", email: "bad" }] },
        original
      )
    ).toThrow()
    expect(() =>
      invoiceProfileRequest(
        {
          ...values,
          tax: [
            { key: "tax_id", value: "a" },
            { key: "tax_id", value: "b" },
          ],
        },
        original
      )
    ).toThrow()
  })
  it("uses server-filtered actions instead of guessing authority from role names", () => {
    expect(
      allowedInvoiceActions({
        available_actions: [],
      } as unknown as MerchantInvoice)
    ).toEqual([])
    expect(
      allowedInvoiceActions({
        available_actions: ["void"],
      } as unknown as MerchantInvoice)
    ).toEqual(["void"])
  })
  it("does not describe failed or uncertain collection as paid", () => {
    const result = (status: string, replayed = false) =>
      ({ attempt: { status }, replayed }) as unknown as InvoiceRetryResponse
    expect(invoiceResultMessage(result("failed"))).toContain("failed")
    expect(invoiceResultMessage(result("attempted"))).toContain(
      "pending verification"
    )
    expect(invoiceResultMessage(result("settled", true))).toBe(
      "Existing payment confirmed."
    )
  })
})
