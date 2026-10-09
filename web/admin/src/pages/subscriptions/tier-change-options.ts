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

export interface TierChangeOption {
  direction: "upgrade" | "downgrade"
  price: Price
  product: Product
}

export function tierChangeOptionLabel(option: TierChangeOption): string {
  const price = option.price
  return [
    option.product.display_name,
    option.direction,
    `${formatNativeAmount(price.unit_amount, price.currency)} ${priceIntervalLabel(price)}`,
    price.key,
  ].join(" · ")
}

export function adminTierChangeBlockReason({
  rail,
  status,
  scheduledChange,
}: {
  rail: Rail
  status: SubscriptionStatus
  scheduledChange?: Pick<ScheduledChange, "source"> | null
}): string | undefined {
  if (status !== "active" && status !== "past_due") {
    return "Only active or past-due subscriptions can change tier"
  }
  if (scheduledChange?.source === "change") return "A tier change is already scheduled"
  if (scheduledChange) return "A price change is already scheduled"
  if (rail === "ccbill")
    return "CCBill tier changes require customer self-service"
  if (rail === "solana") {
    return "Solana tier changes require the customer's wallet signature"
  }
  return undefined
}

export function tierChangeOptions({
  currentProduct,
  currentCurrency,
  products,
  prices,
}: {
  currentProduct?: Product
  currentCurrency?: string
  products: Product[]
  prices: Price[]
}): TierChangeOption[] {
  const tierGroup = currentProduct?.tier_group?.trim()
  const currency = currentCurrency?.trim().toLowerCase()
  if (!currentProduct || !tierGroup || !currency) return []

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

  return prices
    .flatMap((price): TierChangeOption[] => {
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
}
