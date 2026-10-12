// ProductList sells products by key: each one on sale, one buy button per price.
// The host decides what to sell (its server names the products, say from its
// gate's 402) and what happens once it's paid.
import * as React from "react"
import { cn } from "cn"

import { RESET } from "#orck/account/format"
import { EmptyState, ErrorState, ListSkeleton } from "#orck/account/section"
import type { CheckoutAppearance } from "#orck/appearance"
import { Purchase } from "#orck/buy-button"
import type { Order, Product } from "#orck/client/types"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/components/ui/card"
import { useMessages } from "#orck/i18n/context"
import { priceLabel } from "#orck/lib/price-label"
import { useCurrencyScales } from "#orck/react/config"
import { useProducts, type ProductsState } from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"

export interface ProductListProps {
  /** The id of the merchant whose catalog `keys` are looked up in. */
  merchant?: string
  /**
   * What to sell: these product keys of `merchant`, such as a course, its
   * bundle and the membership that also unlocks it, looked up in OpenRails'
   * public catalog. The host's server names them (the Go Client's
   * ListProducts finds the products granting an entitlement).
   */
  keys?: string[]
  /**
   * The products to display instead, from the host's own server (the Go
   * Client's ListProducts): nothing is fetched.
   */
  products?: Product[]
  /**
   * The order is paid. A hint: the host's own gate decides access, so send
   * the buyer back to the content it guards.
   */
  onPaid: (order: Order) => void
  /**
   * Buying needs a signed-in customer: called instead of the checkout when
   * `signedIn` is false or OpenRails answers 401.
   */
  onSignInRequired: () => void
  /** false when the host knows the visitor is signed out. */
  signedIn?: boolean
  appearance?: CheckoutAppearance
  className?: string
}

export function ProductList({ products, ...props }: ProductListProps) {
  return products ? (
    <ProductCards
      {...props}
      state={{ products, error: null, refetch: () => {} }}
    />
  ) : (
    <FetchedProducts {...props} />
  )
}

function FetchedProducts(props: ProductListProps) {
  const state = useProducts({
    merchant: props.merchant ?? "",
    keys: props.keys ?? [],
  })
  return <ProductCards {...props} state={state} />
}

function ProductCards({
  onPaid,
  onSignInRequired,
  signedIn,
  appearance,
  className,
  state,
}: ProductListProps & {
  state: Pick<ProductsState, "products" | "error" | "refetch">
}) {
  const m = useMessages()
  const { locale } = useUiSettings()
  const scope = useScopeProps(appearance)
  const scales = useCurrencyScales()

  // A deposit's amount is chosen before checkout; ProductList sells fixed prices.
  const products = (state.products ?? [])
    .filter((product) => !product.archived)
    .map((product) => ({
      product,
      prices: product.prices.filter((p) => !p.archived && !p.customer_amount),
    }))
    .filter((entry) => entry.prices.length > 0)

  let body: React.ReactNode
  if (state.products === null && state.error) {
    body = <ErrorState error={state.error} onRetry={state.refetch} />
  } else if (state.products === null) {
    body = <ListSkeleton />
  } else if (products.length === 0) {
    body = <EmptyState title={m.t("products.empty")} />
  } else {
    body = products.map(({ product, prices }) => (
      <Card key={product.id} className="gap-4" data-testid="product">
        <CardHeader>
          <CardTitle role="heading" aria-level={2}>
            {product.display_name}
          </CardTitle>
          {product.description ? (
            <CardDescription>{product.description}</CardDescription>
          ) : null}
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          {prices.map((price) => (
            <Purchase
              key={price.id}
              priceId={price.id}
              label={priceLabel(price, scales, m, locale)}
              onPaid={onPaid}
              onSignInRequired={onSignInRequired}
              signedIn={signedIn}
              appearance={appearance}
            />
          ))}
        </CardContent>
      </Card>
    ))
  }

  return (
    <div
      className={cn(scope.className, RESET, "grid gap-4", className)}
      data-orck-theme={scope["data-orck-theme"]}
      style={scope.style}
    >
      {body}
    </div>
  )
}
