// BuyButton buys one price the host's page carries: it creates the signed-in
// customer's invoice and pays it, in InvoicePayment on the page, or, on a site
// the merchant registered to frame its pay URLs, in the invoice's embedded
// pay URL on the checkout origin. The app never handles the invoice itself.
import * as React from "react"
import { cn } from "cn"

import type { BillingAppearance } from "#orck/appearance"
import { isBillingError, localError, toBillingError } from "#orck/client/errors"
import type { Invoice, InvoiceAttribution, Price } from "#orck/client/types"
import type { PaymentFallbacks } from "#orck/fallbacks"
import { Button } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import { priceLabel } from "#orck/lib/price-label"
import { EmbeddedModal, InvoiceModal } from "#orck/modal"
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
   * price with `customer_amount`; the invoice charges exactly this.
   */
  amount?: string
  /** Where a redirect PSP (Stripe's hosted page) returns the buyer: one of the app's return origins. */
  returnUrl?: string
  /** PSPs kept back until others fail on the invoice; see InvoicePayment. */
  fallbacks?: PaymentFallbacks
  /** Where the host's page says the customer came from (utm_*, referrer). */
  attribution?: InvoiceAttribution
  /**
   * The invoice is paid. A hint: the host's own gate decides access, so send
   * the buyer to the content it guards.
   */
  onPaid: (invoice: Invoice) => void
  /**
   * Buying needs a signed-in customer: called instead of paying when
   * `signedIn` is false or OpenRails answers 401.
   */
  onSignInRequired: () => void
  /** false when the host knows the visitor is signed out. */
  signedIn?: boolean
  appearance?: BillingAppearance
  className?: string
}

// An unpaid invoice of the customer's that holds what is being bought: the
// refusal names it, and the customer pays that one instead.
function heldBy(err: unknown): string | undefined {
  if (!isBillingError(err) || err.code !== "already_owned") return undefined
  const owner = err.metadata.owned_by
  return typeof owner === "string" && owner.startsWith("in_")
    ? owner
    : undefined
}

// Where the invoice is paid: on this page, or framed from the checkout origin.
type Paying =
  | { kind: "inline"; invoiceId: string }
  | { kind: "embedded"; invoiceId: string; url: string }

// A page on an origin the merchant registered to frame its pay URLs pays
// there; any other pays on the page.
const framesCheckout = (origins: string[]) =>
  typeof window !== "undefined" && origins.includes(window.location.origin)

/** One price's button and its invoice. Requires `BillingProvider`. */
export function BuyButton({
  price,
  label,
  amount,
  returnUrl,
  fallbacks,
  attribution,
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
  const paid = (invoice: Invoice) => {
    if (invoice.status !== "complete") return
    setPaying(undefined)
    onPaid(invoice)
  }

  const buy = async () => {
    if (signedIn === false) return onSignInRequired()
    setFailure(undefined)
    setStarting(true)
    try {
      const { merchant } = await client.getCustomerConfig()
      const embedded = framesCheckout(merchant.embed_origins)
      let invoice: Invoice
      try {
        // No key: OpenRails answers the customer's open invoice of these lines,
        // so a second click, a reopened dialog or a reload pays the same one.
        ;({ invoice } = await client.createInvoice({
          lines: [{ price_id: price.id, ...(amount ? { amount } : {}) }],
          ...(embedded ? { payUrl: { embedded: true } } : {}),
          ...(attribution ? { attribution } : {}),
        }))
      } catch (err) {
        const held = heldBy(err)
        // Only an invoice made with a pay URL answers its URL.
        if (!held || embedded) throw err
        setPaying({ kind: "inline", invoiceId: held })
        return
      }
      if (invoice.status === "complete") return paid(invoice)
      if (!embedded) {
        setPaying({ kind: "inline", invoiceId: invoice.id })
        return
      }
      const url = invoice.pay_url
      if (!url)
        throw localError(
          "invalid_response",
          "the invoice answered no pay URL"
        )
      setPaying({ kind: "embedded", invoiceId: invoice.id, url })
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
        <InvoiceModal
          open
          onOpenChange={close}
          invoiceId={paying.invoiceId}
          returnUrl={returnUrl}
          fallbacks={fallbacks}
          appearance={appearance}
          onSettled={paid}
        />
      ) : null}
      {paying?.kind === "embedded" ? (
        <EmbeddedModal
          open
          onOpenChange={close}
          url={paying.url}
          fallbacks={fallbacks}
          appearance={appearance}
          // The frame's word is a hint: the invoice says whether it is paid.
          onComplete={() =>
            void client.getInvoice(paying.invoiceId).then(paid, () => undefined)
          }
          onLoadFailure={() =>
            void client
              .reportPayURLFailure(paying.invoiceId)
              .catch(() => undefined)
          }
        />
      ) : null}
    </span>
  )
}
