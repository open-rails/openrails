// Apple Pay and Google Pay through Stripe's Express Checkout Element: the
// wallet makes a payment method (`pm_`) the order charges as it does a new
// card's. Stripe shows a wallet only on a registered payment-method domain,
// in a browser that supports it, with a card in the wallet.
import * as React from "react"
import type {
  StripeExpressCheckoutElementConfirmEvent,
  StripeExpressCheckoutElementReadyEvent,
} from "@stripe/stripe-js"

import { loadStripeFor } from "#orck/lib/stripe"
import { stripeAppearance } from "#orck/lib/stripe-appearance"

export interface StripeWalletsProps {
  publishableKey: string
  /** The total in the currency's minor units (Stripe's `amount`). */
  amount: number
  currency: string
  /** Pays with the wallet's payment method; false when the payment failed. */
  onPaymentMethod: (paymentMethod: string) => Promise<boolean>
  /** Whether a wallet button shows. */
  onAvailable?: (available: boolean) => void
}

export function StripeWallets({
  publishableKey,
  amount,
  currency,
  onPaymentMethod,
  onAvailable,
}: StripeWalletsProps) {
  const host = React.useRef<HTMLDivElement>(null)
  const pay = React.useRef(onPaymentMethod)
  const available = React.useRef(onAvailable)
  React.useEffect(() => {
    pay.current = onPaymentMethod
    available.current = onAvailable
  })
  const [shown, setShown] = React.useState(false)

  React.useEffect(() => {
    if (!host.current) return
    let canceled = false
    let unmount: (() => void) | undefined
    const show = (value: boolean) => {
      if (canceled) return
      setShown(value)
      available.current?.(value)
    }
    void loadStripeFor(publishableKey).then(
      (stripe) => {
        if (canceled || !host.current) return
        const elements = stripe.elements({
          mode: "payment",
          amount,
          currency: currency.toLowerCase(),
          paymentMethodCreation: "manual",
          paymentMethodTypes: ["card"],
          appearance: stripeAppearance(host.current),
        })
        const element = elements.create("expressCheckout", {
          paymentMethods: {
            applePay: "auto",
            googlePay: "auto",
            link: "never",
            paypal: "never",
            amazonPay: "never",
            klarna: "never",
          },
        })
        element.on("ready", (event: StripeExpressCheckoutElementReadyEvent) => {
          const methods = event.availablePaymentMethods
          show(!!methods && (methods.applePay || methods.googlePay))
        })
        element.on(
          "confirm",
          (event: StripeExpressCheckoutElementConfirmEvent) => {
            void (async () => {
              const submitted = await elements.submit()
              const created = submitted.error
                ? undefined
                : await stripe.createPaymentMethod({ elements })
              const method = created?.paymentMethod?.id
              if (!method || !(await pay.current(method)))
                event.paymentFailed({ reason: "fail" })
            })()
          }
        )
        element.mount(host.current)
        unmount = () => element.destroy()
      },
      () => show(false)
    )
    return () => {
      canceled = true
      unmount?.()
    }
  }, [publishableKey, amount, currency])

  return (
    <div
      ref={host}
      data-testid="stripe-wallets"
      style={{ visibility: shown ? "visible" : "hidden" }}
    />
  )
}
