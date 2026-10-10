// Offers sells products by key: each one on sale, one BuyButton per price.
// The host decides what to sell (its server names the products, say from its
// gate's 402) and what happens once it's paid.
import * as React from "react"
import { cn } from "cn"

import { RESET } from "#orck/account/format"
import { EmptyState, ErrorState, ListSkeleton } from "#orck/account/section"
import type { CheckoutAppearance } from "#orck/appearance"
import { BuyButton } from "#orck/buy-button"
import type { Product } from "#orck/client/types"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/components/ui/card"
import { useMessages } from "#orck/i18n/context"
import { offerLabel } from "#orck/lib/offer-label"
import { useCurrencyScales } from "#orck/react/config"
import { useProducts, type ProductsState } from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"
import type { PayResult } from "#orck/types"

export interface OffersProps {
  /**
   * What to sell: these product keys, such as a course, its bundle and the
   * membership that also unlocks it, looked up in OpenRails' public catalog.
   * The host's server names them (the Go Client's ListOffers finds the
   * products granting an entitlement).
   */
  keys?: string[]
  /**
   * The products to offer instead, from the host's own server (the Go
   * Client's ListOffers): nothing is fetched.
   */
  products?: Product[]
  /**
   * The checkout succeeded. A hint: the host's own gate decides access, so
   * send the buyer back to the content it guards.
   */
  onPaid: (result: PayResult) => void
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

export function Offers({ products, ...props }: OffersProps) {
  return products ? (
    <OfferList
      {...props}
      state={{ products, error: null, refetch: () => {} }}
    />
  ) : (
    <FetchedOffers {...props} />
  )
}

function FetchedOffers(props: OffersProps) {
  const state = useProducts({ keys: props.keys ?? [] })
  return <OfferList {...props} state={state} />
}

function OfferList({
  onPaid,
  onSignInRequired,
  signedIn,
  appearance,
  className,
  state,
}: OffersProps & {
  state: Pick<ProductsState, "products" | "error" | "refetch">
}) {
  const m = useMessages()
  const { locale } = useUiSettings()
  const scope = useScopeProps(appearance)
  const scales = useCurrencyScales()

  // A deposit's amount is chosen before checkout; Offers sells fixed prices.
  const offers = (state.products ?? [])
    .map((product) => ({
      product,
      prices: product.prices.filter((p) => !p.archived && !p.customer_amount),
    }))
    .filter((offer) => offer.prices.length > 0)

  let body: React.ReactNode
  if (state.products === null && state.error) {
    body = <ErrorState error={state.error} onRetry={state.refetch} />
  } else if (state.products === null) {
    body = <ListSkeleton />
  } else if (offers.length === 0) {
    body = <EmptyState title={m.t("offers.empty")} />
  } else {
    body = offers.map(({ product, prices }) => (
      <Card key={product.id} className="gap-4" data-testid="offer">
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
            <BuyButton
              key={price.id}
              product={product.key}
              price={price.key}
              label={offerLabel(price, scales, m, locale)}
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
