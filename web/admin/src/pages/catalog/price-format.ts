import type { Price } from "@/lib/api/generated/wire"
import { formatNativeAmount } from "@/lib/format"

// Access duration is stored in hours because that is the unit the engine
// charges on. Nobody reads "744h" as a month, so whole days, weeks and months
// are named; anything that does not divide evenly keeps its hours.
export function durationLabel(hours: number): string {
  const units: [number, string][] = [
    [720, "month"],
    [168, "week"],
    [24, "day"],
  ]
  for (const [size, name] of units) {
    if (hours % size === 0) {
      const count = hours / size
      return `${count} ${name}${count === 1 ? "" : "s"}`
    }
  }
  return `${hours} hour${hours === 1 ? "" : "s"}`
}

// priceIntervalLabel renders a price's renewal cadence — shared between the
// catalog list and the #777 price-change wizard so "currency + interval
// locked" always reads identically in both places.
export function priceIntervalLabel(
  price: Pick<Price, "auto_renew" | "access_duration_hours">
): string {
  if (price.auto_renew) {
    return price.access_duration_hours
      ? `every ${durationLabel(price.access_duration_hours)}`
      : "every period"
  }
  if (price.access_duration_hours) {
    return `${durationLabel(price.access_duration_hours)} once`
  }
  return "one-time"
}

// A deposit descriptor has no fixed amount: zero is not its purchase price.
export function priceAmountLabel(
  price: Pick<Price, "unit_amount" | "currency" | "customer_amount">
): string {
  const range = price.customer_amount
  if (!range) return formatNativeAmount(price.unit_amount, price.currency)
  return `Customer chooses ${formatNativeAmount(range.min_amount, price.currency)}–${formatNativeAmount(range.max_amount, price.currency)}`
}
