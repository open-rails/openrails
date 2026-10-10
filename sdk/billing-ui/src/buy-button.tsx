// BuyButton buys one price: the purchase, the checkout and its payment are
// billing-ui's, so the app never handles a checkout session. Today it opens a
// checkout session in CheckoutModal; with orders (#1168) it will create an
// order instead, and apps won't change.
import * as React from "react"
import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import { isBillingError, toBillingError } from "#orck/client/errors"
import { Button } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import { offerLabel } from "#orck/lib/offer-label"
import { CheckoutModal } from "#orck/modal"
import { useCurrencyScales } from "#orck/react/config"
import { useBillingContext } from "#orck/react/context"
import { useProducts } from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"
import type { PayResult } from "#orck/types"

export interface BuyButtonProps {
  /** The product's catalog key. */
  product: string
  /** The price's key within the product. */
  price: string
  /** The button's text; by default the price's amount and terms, from the catalog. */
  label?: string
  /**
   * The purchase succeeded. A hint: the host's own gate decides access, so
   * send the buyer to the content it guards.
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

export function BuyButton(props: BuyButtonProps) {
  return props.label === undefined ? (
    <CatalogLabel {...props} />
  ) : (
    <Purchase {...props} label={props.label} />
  )
}

// CatalogLabel reads the price from the public catalog to label the button.
function CatalogLabel(props: BuyButtonProps) {
  const m = useMessages()
  const { locale } = useUiSettings()
  const scales = useCurrencyScales()
  const { products } = useProducts({ keys: [props.product] })
  const price = products
    ?.find((p) => p.key === props.product)
    ?.prices.find((p) => p.key === props.price)
  return (
    <Purchase
      {...props}
      label={price ? offerLabel(price, scales, m, locale) : ""}
      disabled={!price}
    />
  )
}

function Purchase({
  product,
  price,
  label,
  onPaid,
  onSignInRequired,
  signedIn,
  appearance,
  className,
  disabled,
}: BuyButtonProps & { label: string; disabled?: boolean }) {
  const m = useMessages()
  const scope = useScopeProps(appearance)
  const { client } = useBillingContext()
  const [session, setSession] = React.useState<string>()
  const [starting, setStarting] = React.useState(false)
  const [failure, setFailure] = React.useState<string>()
  // One source per session: Checkout drops the result of a payment whose
  // source changed, and saving a card re-renders the tree.
  const source = React.useMemo(
    () =>
      session
        ? // The customer's surface: their saved cards pay too.
          client.checkoutSource(session, {
            customerBase: `${client.baseUrl}/me`,
          })
        : null,
    [client, session]
  )

  const buy = async () => {
    if (signedIn === false) return onSignInRequired()
    setFailure(undefined)
    setStarting(true)
    try {
      const { id } = await client.createCheckoutSession({
        productKey: product,
        priceKey: price,
      })
      setSession(id)
    } catch (err) {
      if (isBillingError(err) && err.status === 401) onSignInRequired()
      else setFailure(m.error(toBillingError(err)))
    } finally {
      setStarting(false)
    }
  }

  return (
    <span
      className={cn(scope.className, "inline-flex flex-col gap-1", className)}
      data-orck-theme={scope["data-orck-theme"]}
      // A styling root is a size container; a button sizes to its label.
      style={{ ...scope.style, containerType: "normal" }}
    >
      <Button disabled={disabled || starting} onClick={() => void buy()}>
        {label}
      </Button>
      {failure ? (
        <span role="alert" className="text-sm text-destructive">
          {failure}
        </span>
      ) : null}
      {source ? (
        <CheckoutModal
          open
          onOpenChange={(open) => !open && setSession(undefined)}
          source={source}
          appearance={appearance}
          onComplete={(result) => {
            if (result.status !== "succeeded") return
            setSession(undefined)
            onPaid(result)
          }}
        />
      ) : null}
    </span>
  )
}
