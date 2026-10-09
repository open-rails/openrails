import type { Price } from "@/lib/api/generated/wire"
import { formatHours } from "@/lib/duration"
import { formatNativeAmount } from "@/lib/format"

// priceIntervalLabel renders a price's renewal cadence — shared between the
// catalog list and the #777 price-change wizard so "currency + interval
// locked" always reads identically in both places.
export function priceIntervalLabel(
  price: Pick<Price, "billing_interval_hours" | "access_duration_hours">
): string {
  if (price.billing_interval_hours) {
    return `every ${formatHours(price.billing_interval_hours)}`
  }
  if (price.access_duration_hours) {
    return `${formatHours(price.access_duration_hours)} once`
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
