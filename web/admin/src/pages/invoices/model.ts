import type {
  InvoiceAction,
  InvoiceProfile,
  InvoiceRetryResponse,
  MerchantInvoice,
} from "@/lib/api/invoice-types"
import { unitsFromInput } from "@/lib/format"

export const invoiceActionLabels: Record<InvoiceAction, string> = {
  void: "Void invoice",
  mark_uncollectible: "Mark uncollectible",
  record_payment: "Record payment",
  retry_collection: "Retry collection",
}
export const invoiceActionDescriptions: Record<InvoiceAction, string> = {
  void: "Cancel the unpaid balance. Existing payments and the issued invoice facts remain recorded.",
  mark_uncollectible:
    "Stop scheduled collection. The unpaid balance remains owed.",
  record_payment:
    "Record money already received outside automatic collection. This does not charge a payment method.",
  retry_collection:
    "Retry the unpaid amount using the selected saved payment method. Uncertain or in-progress collections must finish reconciliation first.",
}
export function allowedInvoiceActions(invoice: MerchantInvoice) {
  return invoice.available_actions ?? []
}
export function invoiceResultMessage(
  result: MerchantInvoice | InvoiceRetryResponse
) {
  if ("attempt" in result) {
    if (result.attempt.status === "settled")
      return result.replayed
        ? "Existing payment confirmed."
        : "Invoice payment collected."
    if (result.attempt.status === "failed")
      return "Collection failed. Review the payment history before trying again."
    return "Collection is pending verification. No new collection should be started."
  }
  return "Invoice updated."
}
export function invoicePaymentAmount(
  input: string,
  amountDue: number,
  decimals: number
) {
  const amount = unitsFromInput(input.trim(), decimals)
  if (amount === null)
    throw new Error(
      `Enter a positive amount with up to ${decimals} decimal places.`
    )
  if (
    amount === null ||
    !Number.isSafeInteger(amount) ||
    amount <= 0 ||
    amount > amountDue
  )
    throw new Error(
      "Payment must be positive and no greater than the unpaid balance."
    )
  return amount
}
export interface ProfileFormValues {
  terms: string
  method: InvoiceProfile["collection_method"]
  po: string
  memo: string
  contacts: { name: string; email: string }[]
  tax: { key: string; value: string }[]
}
export const taxDisplay = (value: unknown) =>
  typeof value === "string" ? value : JSON.stringify(value)
export function invoiceProfileValues(
  profile: InvoiceProfile | null
): ProfileFormValues {
  return {
    terms: String(profile?.net_terms_days ?? 0),
    method: profile?.collection_method ?? "charge_automatically",
    po: profile?.po_number ?? "",
    memo: profile?.memo ?? "",
    contacts: (profile?.billing_contacts ?? []).map((c) => ({
      name: c.name ?? "",
      email: c.email,
    })),
    tax: Object.entries(profile?.tax ?? {}).map(([key, value]) => ({
      key,
      value: taxDisplay(value),
    })),
  }
}
export function invoiceProfileRequest(
  values: ProfileFormValues,
  original: InvoiceProfile | null
): InvoiceProfile {
  const terms = Number(values.terms)
  if (
    !/^\d+$/.test(values.terms) ||
    !Number.isSafeInteger(terms) ||
    terms > 2147483647
  )
    throw new Error("Terms must be a non-negative whole number of days.")
  const contacts = values.contacts
    .filter((c) => c.name.trim() || c.email.trim())
    .map((c) => ({ name: c.name.trim(), email: c.email.trim() }))
  if (contacts.some((c) => !/^\S+@\S+$/.test(c.email)))
    throw new Error("Each billing contact needs a valid email address.")
  const rows = values.tax.filter((row) => row.key.trim() || row.value.trim())
  const keys = rows.map((row) => row.key.trim())
  if (keys.some((k) => !k) || new Set(keys).size !== keys.length)
    throw new Error("Tax details need distinct, non-empty names.")
  const tax = Object.fromEntries(
    rows.map((row) => {
      const key = row.key.trim(),
        old = original?.tax?.[key]
      if (old !== undefined && typeof old !== "string") {
        if (row.value === taxDisplay(old)) return [key, old]
        try {
          return [key, JSON.parse(row.value)]
        } catch {
          throw new Error(`Invalid structured tax detail: ${key}`)
        }
      }
      return [key, row.value.trim()]
    })
  )
  return {
    net_terms_days: terms,
    collection_method: values.method,
    po_number: values.po.trim(),
    memo: values.memo.trim(),
    billing_contacts: contacts,
    tax,
  }
}
