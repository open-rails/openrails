// BuyButton buys one price the host's page carries: it creates the signed-in
// customer's order and pays it, in OrderCheckout on the page, or, on a site
// the merchant registered to frame its checkout, in the order's embedded
// checkout on the checkout host. The app never handles the order itself.
import * as React from "react"
import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import { isBillingError, localError, toBillingError } from "#orck/client/errors"
import type { Order, Price } from "#orck/client/types"
import { Button } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import { priceLabel } from "#orck/lib/price-label"
import { EmbeddedModal, OrderModal } from "#orck/modal"
import { useCurrencyScales } from "#orck/react/config"
import { useBillingContext } from "#orck/react/context"
import { useScopeProps, useUiSettings } from "#orck/scope-context"

export interface BuyButtonProps {
  /**
   * The price to buy, from a product the host's server put in its page (the
   * Go Client's ListProducts, its prices as a buyer sees them:
   * `billing.BuyerPrices`). The browser never reads the catalog.
   */
  price: Price
  /** The button's text; by default the price's amount and terms. */
  label?: string
  /**
   * A customer-chosen deposit, in the price currency's native units, for a
   * price with `customer_amount`; the order charges exactly this.
   */
  amount?: string
  /** Where a redirect PSP (Stripe's hosted page) returns the buyer: one of the app's return origins. */
  returnUrl?: string
  /**
   * The order is paid. A hint: the host's own gate decides access, so send
   * the buyer to the content it guards.
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

// An unpaid order of the customer's that holds what is being bought: the
// refusal names it, and the customer pays that one instead.
function heldBy(err: unknown): string | undefined {
  if (!isBillingError(err) || err.code !== "already_owned") return undefined
  const owner = err.metadata.owned_by
  return typeof owner === "string" && owner.startsWith("ord_")
    ? owner
    : undefined
}

// Where the order is paid: on this page, or framed from the checkout host.
type Paying =
  | { kind: "inline"; orderId: string }
  | { kind: "embedded"; orderId: string; url: string }

// A page on an origin the merchant registered to frame its checkout pays
// there; any other pays on the page.
const framesPayment = (origins: string[]) =>
  typeof window !== "undefined" && origins.includes(window.location.origin)

/** One price's button and its order. Requires `BillingProvider`. */
export function BuyButton({
  price,
  label,
  amount,
  returnUrl,
  onPaid,
  onSignInRequired,
  signedIn,
  appearance,
  className,
}: BuyButtonProps) {
  const m = useMessages()
  const { locale } = useUiSettings()
  const scales = useCurrencyScales()
  const scope = useScopeProps(appearance)
  const { client } = useBillingContext()
  const [paying, setPaying] = React.useState<Paying>()
  const [starting, setStarting] = React.useState(false)
  const [failure, setFailure] = React.useState<string>()
  const paid = (order: Order) => {
    if (order.status !== "complete") return
    setPaying(undefined)
    onPaid(order)
  }

  const buy = async () => {
    if (signedIn === false) return onSignInRequired()
    setFailure(undefined)
    setStarting(true)
    try {
      const { merchant } = await client.getCustomerConfig()
      const embedded = framesPayment(merchant.embed_origins)
      let order: Order
      try {
        // No key: OpenRails answers the customer's open order of these lines,
        // so a second click, a reopened dialog or a reload pays the same one.
        ;({ order } = await client.createOrder({
          lines: [{ price_id: price.id, ...(amount ? { amount } : {}) }],
          ...(embedded ? { checkout: { ui_mode: "embedded" } } : {}),
        }))
      } catch (err) {
        const held = heldBy(err)
        // Only an order made with a checkout answers its URL.
        if (!held || embedded) throw err
        setPaying({ kind: "inline", orderId: held })
        return
      }
      if (order.status === "complete") return paid(order)
      if (!embedded) {
        setPaying({ kind: "inline", orderId: order.id })
        return
      }
      const url = order.checkout?.url
      if (!url)
        throw localError(
          "invalid_response",
          "the order answered no checkout URL"
        )
      setPaying({ kind: "embedded", orderId: order.id, url })
    } catch (err) {
      if (isBillingError(err) && err.status === 401) onSignInRequired()
      else setFailure(m.error(toBillingError(err)))
    } finally {
      setStarting(false)
    }
  }

  const close = (open: boolean) => !open && setPaying(undefined)
  return (
    <span
      className={cn(scope.className, "inline-flex flex-col gap-1", className)}
      data-orck-theme={scope["data-orck-theme"]}
      // A styling root is a size container; a button sizes to its label.
      style={{ ...scope.style, containerType: "normal" }}
    >
      <Button disabled={starting} onClick={() => void buy()}>
        {label ?? priceLabel(price, scales, m, locale)}
      </Button>
      {failure ? (
        <span role="alert" className="text-sm text-destructive">
          {failure}
        </span>
      ) : null}
      {paying?.kind === "inline" ? (
        <OrderModal
          open
          onOpenChange={close}
          orderId={paying.orderId}
          returnUrl={returnUrl}
          appearance={appearance}
          onSettled={paid}
        />
      ) : null}
      {paying?.kind === "embedded" ? (
        <EmbeddedModal
          open
          onOpenChange={close}
          url={paying.url}
          appearance={appearance}
          // The frame's word is a hint: the order says whether it is paid.
          onComplete={() =>
            void client.getOrder(paying.orderId).then(paid, () => undefined)
          }
        />
      ) : null}
    </span>
  )
}
