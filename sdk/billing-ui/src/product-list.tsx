// ProductList sells the products the host's page carries: each one on sale,
// one buy button per price. The host's server decides what to sell (say from
// its gate's 402) and puts the products in its page; the browser never reads
// the catalog. The host decides what happens once it's paid.
import * as React from "react"
import { cn } from "cn"

import { RESET } from "#orck/account/format"
import { EmptyState } from "#orck/account/section"
import type { BillingAppearance } from "#orck/appearance"
import { BuyButton } from "#orck/buy-button"
import type { Order, Product } from "#orck/client/types"
import type { PaymentFallbacks } from "#orck/fallbacks"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/components/ui/card"
import { useMessages } from "#orck/i18n/context"
import { useScopeProps } from "#orck/scope-context"

export interface ProductListProps {
  /**
   * What to sell, from the host's own server: the Go Client's ListProducts
   * (by product key or entitlement), their prices as a buyer sees them
   * (`billing.BuyerPrices`).
   */
  products: Product[]
  /**
   * The order is paid. A hint: the host's own gate decides access, so send
   * the buyer back to the content it guards.
   */
  onPaid: (order: Order) => void
  /**
   * Buying needs a signed-in customer: called instead of the purchase when
   * `signedIn` is false or OpenRails answers 401.
   */
  onSignInRequired: () => void
  /** false when the host knows the visitor is signed out. */
  signedIn?: boolean
  /** Where a redirect PSP (Stripe's hosted page) returns the buyer: one of the app's return origins. */
  returnUrl?: string
  /** PSPs kept back until others fail on the order; see OrderPayment. */
  fallbacks?: PaymentFallbacks
  appearance?: BillingAppearance
  className?: string
}

type ListedPrice = ProductListProps["products"][number]["prices"][number]

const sameTerms = (a: ListedPrice, b: ListedPrice) =>
  a.unit_amount === b.unit_amount &&
  a.currency === b.currency &&
  a.billing_interval_hours === b.billing_interval_hours &&
  a.access_duration_hours === b.access_duration_hours &&
  a.trial_unit_amount === b.trial_unit_amount &&
  a.trial_duration_hours === b.trial_duration_hours &&
  !a.quantity && !b.quantity

export function ProductList({
  products: given,
  onPaid,
  onSignInRequired,
  signedIn,
  returnUrl,
  fallbacks,
  appearance,
  className,
}: ProductListProps) {
  const m = useMessages()
  const scope = useScopeProps(appearance)

  // A deposit's amount is chosen before purchase; ProductList sells fixed
  // prices. A provider's price on the terms of one of ours is that price's
  // fallback on the order, not a price of its own.
  const products = given
    .filter((product) => !product.archived)
    .map((product) => {
      const live = product.prices.filter(
        (p) => !p.archived && !p.customer_amount && (!p.provider || p.provider.rail === "ccbill")
      )
      return {
        product,
        prices: live.filter(
          (p) => !p.provider || !live.some((o) => !o.provider && (!o.sell_via?.length || o.sell_via.includes("card")) && sameTerms(o, p))
        ),
      }
    })
    .filter((entry) => entry.prices.length > 0)

  let body: React.ReactNode
  if (products.length === 0) {
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
            <BuyButton
              key={price.id}
              price={price}
              onPaid={onPaid}
              onSignInRequired={onSignInRequired}
              signedIn={signedIn}
              returnUrl={returnUrl}
              fallbacks={fallbacks}
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
