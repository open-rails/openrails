// Card-network identity: which marks we ship and how a rail's spelling maps
// onto them.
export type CardBrand = "visa" | "mastercard" | "amex"

const BRAND_ALIASES: Record<string, CardBrand> = {
  visa: "visa",
  mastercard: "mastercard",
  mc: "mastercard",
  master: "mastercard",
  amex: "amex",
  americanexpress: "amex",
  "american express": "amex",
}

// How people write each brand; a brand not here is shown capitalized.
const BRAND_LABELS: Record<string, string> = {
  visa: "Visa",
  mastercard: "Mastercard",
  amex: "Amex",
  discover: "Discover",
  jcb: "JCB",
  diners: "Diners Club",
  unionpay: "UnionPay",
}

// cardBrandLabel is the rail's spelling of a brand ("VISA", "mc",
// "american_express") as people read it; undefined when there is none.
export function cardBrandLabel(brand?: string | null): string | undefined {
  const value = brand?.trim()
  if (!value) return undefined
  const key = value.toLowerCase().replace(/[^a-z]/g, "")
  return (
    BRAND_LABELS[resolveCardBrand(value) ?? key] ??
    value[0].toUpperCase() + value.slice(1).toLowerCase()
  )
}

// Resolves the rail's spelling of a brand ("Visa", "MASTERCARD", "amex") onto
// the marks we ship; anything unknown falls back to a text plate.
export function resolveCardBrand(brand?: string): CardBrand | undefined {
  const key = brand
    ?.trim()
    .toLowerCase()
    .replace(/[^a-z ]/g, "")
  return key ? BRAND_ALIASES[key] : undefined
}
