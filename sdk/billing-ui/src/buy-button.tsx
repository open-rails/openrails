// BuyButton buys one price: it creates the signed-in customer's order and
// pays it in OrderModal, so the app never handles the order itself.
import * as React from "react"
import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import { isBillingError, toBillingError } from "#orck/client/errors"
import type { Order } from "#orck/client/types"
import { Button } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import { priceLabel } from "#orck/lib/price-label"
import { OrderModal } from "#orck/modal"
import { useCurrencyScales } from "#orck/react/config"
import { useBillingContext } from "#orck/react/context"
import { useProducts } from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"

export interface BuyButtonProps {
  /** The id of the merchant that sells the product. */
  merchant: string
  /** The product's catalog key. */
  product: string
  /** The price's key within the product. */
  price: string
  /** The button's text; by default the price's amount and terms, from the catalog. */
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

export function BuyButton({
  merchant,
  product,
  price: priceKey,
  ...props
}: BuyButtonProps) {
  const m = useMessages()
  const { locale } = useUiSettings()
  const scales = useCurrencyScales()
  const { products } = useProducts({ merchant, keys: [product] })
  const price = products
    ?.find((p) => p.key === product)
    ?.prices.find((p) => p.key === priceKey)
  return (
    <Purchase
      {...props}
      priceId={price?.id}
      label={props.label ?? (price ? priceLabel(price, scales, m, locale) : "")}
    />
  )
}

export type PurchaseProps = Omit<
  BuyButtonProps,
  "merchant" | "product" | "price"
> & {
  /** The price to buy; the button waits while it is unknown. */
  priceId?: string
  label: string
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

/** One price's button and its order. Requires `BillingProvider`. */
export function Purchase({
  priceId,
  label,
  amount,
  returnUrl,
  onPaid,
  onSignInRequired,
  signedIn,
  appearance,
  className,
}: PurchaseProps) {
  const m = useMessages()
  const scope = useScopeProps(appearance)
  const { client } = useBillingContext()
  const [orderId, setOrderId] = React.useState<string>()
  const [starting, setStarting] = React.useState(false)
  const [failure, setFailure] = React.useState<string>()
  // One key per purchase: a second click before it is paid answers the same
  // order instead of creating another.
  const [key, setKey] = React.useState(() => crypto.randomUUID())

  const buy = async () => {
    if (signedIn === false) return onSignInRequired()
    if (!priceId) return
    setFailure(undefined)
    setStarting(true)
    try {
      const { order } = await client.createOrder({
        lines: [{ price_id: priceId, ...(amount ? { amount } : {}) }],
        idempotencyKey: key,
      })
      setOrderId(order.id)
    } catch (err) {
      const held = heldBy(err)
      if (held) setOrderId(held)
      else if (isBillingError(err) && err.status === 401) onSignInRequired()
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
      <Button
        disabled={(signedIn !== false && !priceId) || starting}
        onClick={() => void buy()}
      >
        {label}
      </Button>
      {failure ? (
        <span role="alert" className="text-sm text-destructive">
          {failure}
        </span>
      ) : null}
      {orderId ? (
        <OrderModal
          open
          onOpenChange={(open) => !open && setOrderId(undefined)}
          orderId={orderId}
          returnUrl={returnUrl}
          appearance={appearance}
          onSettled={(order) => {
            if (order.status !== "complete") return
            setOrderId(undefined)
            setKey(crypto.randomUUID())
            onPaid(order)
          }}
        />
      ) : null}
    </span>
  )
}
