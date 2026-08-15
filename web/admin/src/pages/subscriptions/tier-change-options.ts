import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"

export interface TierChangeOption {
  direction: "upgrade" | "downgrade"
  price: CatalogPrice
  product: CatalogProduct
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
