import type { InvoiceProfile } from "#orck/client/generated/wire"

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
      name: c.name,
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
