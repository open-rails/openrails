// Stripe Payment Element that saves a card in one call: Stripe's frames make
// a payment method, and OpenRails saves it to the customer
// (POST /v1/me/payment-methods). A bank that asks for 3-D Secure is answered
// in the page (handleNextAction), then the save is confirmed.
import * as React from "react"
import type {
  Stripe,
  StripeElements,
  StripePaymentElementChangeEvent,
} from "@stripe/stripe-js"

import type { BillingClient } from "#orck/client/client"
import { loadStripeFor } from "#orck/lib/stripe"
import { stripeAppearance } from "#orck/lib/stripe-appearance"
import type { PspConfig } from "#orck/psp"

export interface StripeCardHandle {
  /** Saves the entered card and returns its payment method id. */
  save(): Promise<string>
}

export const StripeCardEntry = React.forwardRef<
  StripeCardHandle,
  {
    psp: PspConfig
    client: BillingClient
    defaultCountry?: string
    /** Completeness of the entered card, from the element's change events. */
    onCompleteChange?: (complete: boolean) => void
    unavailableMessage: string
    verificationPendingMessage: string
  }
>(function StripeCardEntry(
  {
    psp,
    client,
    defaultCountry,
    onCompleteChange,
    unavailableMessage,
    verificationPendingMessage,
  },
  ref
) {
  const [error, setError] = React.useState<string>()
  const host = React.useRef<HTMLDivElement>(null)
  const mounted = React.useRef<{ stripe: Stripe; elements: StripeElements }>(
    null
  )
  const completeRef = React.useRef(onCompleteChange)
  React.useEffect(() => {
    completeRef.current = onCompleteChange
  })

  const publishable = psp.config?.publishable_key
  React.useEffect(() => {
    if (!publishable || !host.current) return
    let canceled = false
    let unmount: (() => void) | undefined
    void loadStripeFor(publishable).then(
      (stripe) => {
        if (canceled || !host.current) return
        const elements = stripe.elements({
          mode: "setup",
          paymentMethodTypes: ["card"],
          paymentMethodCreation: "manual",
          appearance: stripeAppearance(host.current),
        })
        // Cards only: the panel saves a card, so no Link bank or wallets.
        const element = elements.create("payment", {
          wallets: { link: "never", applePay: "never", googlePay: "never" },
          ...(defaultCountry
            ? {
                defaultValues: {
                  billingDetails: { address: { country: defaultCountry } },
                },
              }
            : {}),
        })
        element.on("change", (event: StripePaymentElementChangeEvent) => {
          completeRef.current?.(event.complete)
        })
        element.mount(host.current)
        mounted.current = { stripe, elements }
        unmount = () => element.destroy()
      },
      (cause: unknown) => {
        if (!canceled)
          setError(cause instanceof Error ? cause.message : unavailableMessage)
      }
    )
    return () => {
      canceled = true
      mounted.current = null
      unmount?.()
    }
  }, [publishable, defaultCountry, unavailableMessage])

  React.useImperativeHandle(
    ref,
    () => ({
      async save() {
        const current = mounted.current
        if (!current) throw new Error(unavailableMessage)
        setError(undefined)
        const validation = await current.elements.submit()
        if (validation.error)
          throw new Error(validation.error.message ?? unavailableMessage)
        const created = await current.stripe.createPaymentMethod({
          elements: current.elements,
        })
        if (created.error || !created.paymentMethod)
          throw new Error(created.error?.message ?? unavailableMessage)
        let method = await client.addPaymentMethod({
          psp_id: psp.psp_id,
          token: created.paymentMethod.id,
        })
        const secret = method.next_action?.payload?.client_secret
        if (method.status === "requires_action" && secret) {
          const answered = await current.stripe.handleNextAction({
            clientSecret: secret,
          })
          if (answered.error)
            throw new Error(answered.error.message ?? unavailableMessage)
          method = await client.confirmPaymentMethod(method.id)
        }
        if (method.status === "requires_action")
          throw new Error(verificationPendingMessage)
        return method.id
      },
    }),
    [client, psp.psp_id, unavailableMessage, verificationPendingMessage]
  )

  return (
    <div className="grid gap-2">
      <div ref={host} data-testid="stripe-card-entry" />
      {error ? (
        <p role="alert" className="text-[13px] text-destructive">
          {error}
        </p>
      ) : null}
    </div>
  )
})
