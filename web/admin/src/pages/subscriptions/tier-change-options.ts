import type {
  CatalogPrice,
  CatalogProduct,
  Rail,
  SubscriptionStatus,
} from "@/lib/api/types"
import { formatMicros } from "@/lib/format"
import { priceIntervalLabel } from "@/pages/catalog/price-format"

export interface TierChangeOption {
  direction: "upgrade" | "downgrade"
  price: CatalogPrice
  product: CatalogProduct
}

export function tierChangeOptionLabel(option: TierChangeOption): string {
  const price = option.price
  return [
    option.product.display_name,
    option.direction,
    `${formatMicros(price.unit_amount, price.currency)} ${priceIntervalLabel(price)}`,
    price.key,
  ].join(" · ")
}

export function adminTierChangeBlockReason({
  rail,
  status,
  scheduledPriceId,
  hasPendingReprice,
}: {
  rail: Rail
  status: SubscriptionStatus
  scheduledPriceId?: string | null
  hasPendingReprice: boolean
}): string | undefined {
  if (status !== "active" && status !== "past_due") {
    return "Only active or past-due subscriptions can change tier"
  }
  if (scheduledPriceId) return "A tier change is already scheduled"
  if (hasPendingReprice) return "A price change is already scheduled"
  if (rail === "ccbill") return "CCBill tier changes require customer self-service"
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
  currentProduct?: CatalogProduct
  currentCurrency?: string
  products: CatalogProduct[]
  prices: CatalogPrice[]
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
        !price.auto_renew ||
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
