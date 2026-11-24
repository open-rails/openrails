import type {
  Price,
  ProductSummary,
  ScheduledChange,
  SubscriptionChangeOption as ListedChangeOption,
} from "#orck/client/generated/wire"
import { formatNativeAmount } from "#orck/admin/lib/format"
import { priceIntervalLabel } from "#orck/admin/pages/catalog/price-format"

export interface SubscriptionChangeOption {
  direction: "current" | "upgrade" | "downgrade"
  price: Price
  product: ProductSummary
}

export function subscriptionChangeOptionLabel(
  option: SubscriptionChangeOption
): string {
  const price = option.price
  const amount = `${formatNativeAmount(price.unit_amount, price.currency)}${price.quantity ? " per seat" : ""}`
  return [
    option.product.display_name,
    option.direction,
    `${amount} ${priceIntervalLabel(price)}`,
    price.key,
  ].join(" · ")
}

// The seats a change starts from: the subscription's when the price allows
// them, otherwise the price's minimum.
export function initialSeats(
  price: Price,
  current: number | null
): number | null {
  const bounds = price.quantity
  if (!bounds) return null
  if (current !== null && current >= bounds.min && current <= bounds.max) {
    return current
  }
  return bounds.min
}

// The changes the console offers: the current price, for its seats or to
// change back from a scheduled change, then the prices OpenRails lists the
// subscription can change to, in its order.
export function subscriptionChangeOptions({
  product,
  price,
  scheduledChange,
  targets,
}: {
  product?: ProductSummary
  price?: Price
  scheduledChange?: Pick<ScheduledChange, "source"> | null
  targets: ListedChangeOption[]
}): SubscriptionChangeOption[] {
  const current: SubscriptionChangeOption[] =
    product && price && (price.quantity || scheduledChange)
      ? [{ direction: "current", price, product }]
      : []
  return current.concat(
    targets.map((target) => ({
      direction:
        product && target.product.tier_rank < product.tier_rank
          ? "downgrade"
          : "upgrade",
      price: target.price,
      product: target.product,
    }))
  )
}
