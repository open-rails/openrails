import type { CatalogPrice } from "@/lib/api/types"

// priceIntervalLabel renders a price's renewal cadence — shared between the
// catalog list and the #777 price-change wizard so "currency + interval
// locked" always reads identically in both places.
export function priceIntervalLabel(
  price: Pick<CatalogPrice, "auto_renew" | "access_duration_hours">,
): string {
  if (price.auto_renew) return `every ${price.access_duration_hours ?? "?"}h`
  if (price.access_duration_hours) return `${price.access_duration_hours}h once`
  return "one-time"
}
