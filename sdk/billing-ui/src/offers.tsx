// Offers sells an entitlement: every product on sale that grants it, one
// button per price, each opening the checkout. The host decides what to sell
// (its gate's 402, or ContentKit's paywall) and what happens once it's paid.
import * as React from "react"
import { cn } from "cn"

import { RESET, formatMoney } from "#orck/account/format"
import { EmptyState, ErrorState, ListSkeleton } from "#orck/account/section"
import type { CheckoutAppearance } from "#orck/appearance"
import { isBillingError, toBillingError } from "#orck/client/errors"
import type { Price, Product } from "#orck/client/types"
import { Button } from "#orck/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/components/ui/card"
import { useMessages } from "#orck/i18n/context"
import type { Translator } from "#orck/i18n/messages"
import { periodOf } from "#orck/lib/period"
import { CheckoutModal } from "#orck/modal"
import { useCurrencyScales } from "#orck/react/config"
import { useBillingContext } from "#orck/react/context"
import { useProducts, type ProductsState } from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"
import type { PayResult } from "#orck/types"
import type { CurrencyScales } from "#orck/client/types"

export interface OffersProps {
  /**
   * The entitlement to sell: every product on sale granting it is offered,
   * read from OpenRails' public catalog.
   */
  entitlement?: string
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
  const state = useProducts({ entitlement: props.entitlement })
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
  const { client } = useBillingContext()
  const scales = useCurrencyScales()
  const [session, setSession] = React.useState<string>()
  const [starting, setStarting] = React.useState<string>()
  const [failure, setFailure] = React.useState<string>()

  const buy = async (price: Price) => {
    if (signedIn === false) return onSignInRequired()
    setFailure(undefined)
    setStarting(price.id)
    try {
      setSession((await client.createCheckoutSession({ priceId: price.id })).id)
    } catch (err) {
      if (isBillingError(err) && err.status === 401) onSignInRequired()
      else setFailure(m.error(toBillingError(err)))
    } finally {
      setStarting(undefined)
    }
  }

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
            <Button
              key={price.id}
              disabled={starting !== undefined}
              onClick={() => void buy(price)}
            >
              {offerLabel(price, scales, m, locale)}
            </Button>
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
      {failure ? (
        <p role="alert" className="text-sm text-destructive">
          {failure}
        </p>
      ) : null}
      {session ? (
        <CheckoutModal
          open
          onOpenChange={(open) => !open && setSession(undefined)}
          // The customer's surface: their saved cards pay too.
          source={client.checkoutSource(session, {
            customerBase: `${client.baseUrl}/me`,
          })}
          appearance={appearance}
          onComplete={(result) => {
            if (result.status !== "succeeded") return
            setSession(undefined)
            onPaid(result)
          }}
        />
      ) : null}
    </div>
  )
}

/** "$4.99", "Rent for 3 days, $1.99" or "$10.00 every 30 days". */
function offerLabel(
  price: Pick<
    Price,
    | "unit_amount"
    | "currency"
    | "billing_interval_hours"
    | "access_duration_hours"
  >,
  scales: CurrencyScales,
  m: Translator,
  locale?: string
): string {
  const amount =
    formatMoney(price.unit_amount, price.currency, scales, locale) ??
    `${price.unit_amount} ${price.currency}`
  const every = periodOf(price.billing_interval_hours)
  if (every)
    return m.t("offers.recurring", {
      amount,
      every: m.plural(`interval.every.${every.unit}`, every.count),
    })
  const access = periodOf(price.access_duration_hours)
  if (access)
    return m.t("offers.rent", {
      amount,
      period: m.plural(`interval.duration.${access.unit}`, access.count),
    })
  return m.t("offers.once", { amount })
}
