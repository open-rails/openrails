import type { Price } from "#orck/client/generated/wire"
import { formatHours } from "#orck/admin/lib/duration"
import { formatNativeAmount } from "#orck/admin/lib/format"

// priceIntervalLabel renders a price's renewal cadence, shared by the catalog
// list and the price-change wizard.
export function priceIntervalLabel(
  price: Pick<Price, "billing_interval_hours" | "access_duration_hours"> & { billing_interval_months?: number | null }
): string {
  if (price.billing_interval_months) {
    const months = price.billing_interval_months
    return `every ${months} calendar ${months === 1 ? "month" : "months"}`
  }
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
