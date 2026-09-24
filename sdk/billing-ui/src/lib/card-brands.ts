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

// Resolves the rail's spelling of a brand ("Visa", "MASTERCARD", "amex") onto
// the marks we ship; anything unknown falls back to a text plate.
export function resolveCardBrand(brand?: string): CardBrand | undefined {
  const key = brand
    ?.trim()
    .toLowerCase()
    .replace(/[^a-z ]/g, "")
  return key ? BRAND_ALIASES[key] : undefined
}
