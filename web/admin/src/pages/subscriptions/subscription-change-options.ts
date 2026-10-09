import type {
  Rail,
  SubscriptionStatus,
} from "@/lib/api/types"
import type {
  Price,
  Product,
  ScheduledChange,
} from "@/lib/api/generated/wire"
import { formatNativeAmount } from "@/lib/format"
import { priceIntervalLabel } from "@/pages/catalog/price-format"

export interface SubscriptionChangeOption {
  direction: "current" | "upgrade" | "downgrade"
  price: Price
  product: Product
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
export function initialSeats(price: Price, current: number | null): number | null {
  const bounds = price.quantity
  if (!bounds) return null
  if (current !== null && current >= bounds.min && current <= bounds.max) {
    return current
  }
  return bounds.min
}

export function adminSubscriptionChangeBlockReason({
  rail,
  status,
  collectionPolicy,
  scheduledChange,
}: {
  rail: Rail
  status: SubscriptionStatus
  collectionPolicy?: string
  scheduledChange?: Pick<ScheduledChange, "source"> | null
}): string | undefined {
  if (status !== "active" && status !== "past_due") {
    return "Only active or past-due subscriptions can change"
  }
  // OpenRails changes back only the scheduled change it bills itself.
  if (scheduledChange && collectionPolicy !== "engine") {
    return "Its provider already bills a scheduled change"
  }
  if (rail === "ccbill")
    return "CCBill changes require customer self-service"
  if (rail === "solana") {
    return "Solana changes require the customer's wallet signature"
  }
  return undefined
}

// The changes the console offers: the current price, for its seats or to
// change back from a scheduled change, then, unless a price migration is
// pending, live recurring prices of the tier group in the same currency.
export function subscriptionChangeOptions({
  currentProduct,
  currentPrice,
  currentCurrency,
  products,
  prices,
  scheduledChange,
}: {
  currentProduct?: Product
  currentPrice?: Price
  currentCurrency?: string
  products: Product[]
  prices: Price[]
  scheduledChange?: Pick<ScheduledChange, "source"> | null
}): SubscriptionChangeOption[] {
  const current: SubscriptionChangeOption[] =
    currentProduct && currentPrice && (currentPrice.quantity || scheduledChange)
      ? [{ direction: "current", price: currentPrice, product: currentProduct }]
      : []
  const tierGroup = currentProduct?.tier_group?.trim()
  const currency = currentCurrency?.trim().toLowerCase()
  if (!currentProduct || !tierGroup || !currency || scheduledChange?.source === "migration") {
    return current
  }

  const productsByID = new Map(
    products
      .filter(
        (product) =>
          !product.archived &&
          product.id !== currentProduct.id &&
          product.tier_group?.trim() === tierGroup
      )
      .map((product) => [product.id, product])
  )

  return current.concat(
    prices
      .flatMap((price): SubscriptionChangeOption[] => {
        const product = productsByID.get(price.product_id)
        if (
          !product ||
          price.archived ||
          !price.billing_interval_hours ||
          price.currency.trim().toLowerCase() !== currency
        ) {
          return []
        }
        return [
          {
            direction:
              product.tier_rank < currentProduct.tier_rank
                ? "downgrade"
                : "upgrade",
            price,
            product,
          },
        ]
      })
      .sort(
        (left, right) =>
          left.product.tier_rank - right.product.tier_rank ||
          left.product.display_name.localeCompare(right.product.display_name) ||
          left.price.id.localeCompare(right.price.id)
      )
  )
}
