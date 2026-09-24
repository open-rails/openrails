// Stripe Payment Element on an OpenRails card setup (SetupIntent). The card
// lives only in Stripe's frames; save() confirms the setup in the page (3-D
// Secure included) and returns the saved OpenRails payment method id.
import * as React from "react"
import type {
  Stripe,
  StripeElements,
  StripePaymentElementChangeEvent,
} from "@stripe/stripe-js"

import type { BillingClient } from "#orck/client/client"
import type { CardSetup } from "#orck/client/types"
import { loadStripeFor } from "#orck/lib/stripe"
import { stripeAppearance } from "#orck/lib/stripe-appearance"
import type { PspConfig } from "#orck/psp"

export interface StripeCardHandle {
  /** Confirms the setup and returns the saved payment method id. */
  save(): Promise<string>
}

const defaultReturnURL = (setupId: string) => {
  const url = new URL(window.location.href)
  url.searchParams.set("setup_id", setupId)
  return url.href
}

export const StripeCardEntry = React.forwardRef<
  StripeCardHandle,
  {
    psp: PspConfig
    client: BillingClient
    returnURL?: (setupId: string) => string
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
    returnURL = defaultReturnURL,
    defaultCountry,
    onCompleteChange,
    unavailableMessage,
    verificationPendingMessage,
  },
  ref
) {
  const [idempotencyKey] = React.useState(() => crypto.randomUUID())
  const [setup, setSetup] = React.useState<CardSetup>()
  const [error, setError] = React.useState<string>()
  const host = React.useRef<HTMLDivElement>(null)
  const mounted = React.useRef<{ stripe: Stripe; elements: StripeElements }>(
    null
  )
  const completeRef = React.useRef(onCompleteChange)
  React.useEffect(() => {
    completeRef.current = onCompleteChange
  })

  React.useEffect(() => {
    let cancelled = false
    client
      .createCardSetup({ pspId: psp.psp_id, idempotencyKey })
      .then((created) => {
        if (cancelled) return
        if (!created.client_secret && !created.payment_method_id)
          throw new Error(unavailableMessage)
        setSetup(created)
        if (created.payment_method_id) completeRef.current?.(true)
      })
      .catch((cause: unknown) => {
        if (!cancelled)
          setError(cause instanceof Error ? cause.message : unavailableMessage)
      })
    return () => {
      cancelled = true
    }
  }, [client, psp.psp_id, idempotencyKey, unavailableMessage])

  const secret = setup?.client_secret
  const publishable = psp.config?.publishable_key
  React.useEffect(() => {
    if (!secret || !publishable || !host.current) return
    let cancelled = false
    let unmount: (() => void) | undefined
    void loadStripeFor(publishable).then(
      (stripe) => {
        if (cancelled || !host.current) return
        const elements = stripe.elements({
          clientSecret: secret,
          appearance: stripeAppearance(host.current),
        })
        const element = elements.create("payment", {
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
        if (!cancelled)
          setError(cause instanceof Error ? cause.message : unavailableMessage)
      }
    )
    return () => {
      cancelled = true
      mounted.current = null
      unmount?.()
    }
  }, [secret, publishable, defaultCountry, unavailableMessage])

  React.useImperativeHandle(
    ref,
    () => ({
      async save() {
        if (setup?.payment_method_id) return setup.payment_method_id
        const current = mounted.current
        if (!setup || !current) throw new Error(unavailableMessage)
        setError(undefined)
        const validation = await current.elements.submit()
        if (validation.error)
          throw new Error(validation.error.message ?? unavailableMessage)
        const result = await current.stripe.confirmSetup({
          elements: current.elements,
          confirmParams: { return_url: returnURL(setup.id) },
          redirect: "if_required",
        })
        if (result.error)
          throw new Error(result.error.message ?? unavailableMessage)
        const verified = await client.confirmCardSetup(setup.id)
        if (!verified.payment_method_id)
          throw new Error(verificationPendingMessage)
        return verified.payment_method_id
      },
    }),
    [setup, client, returnURL, unavailableMessage, verificationPendingMessage]
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
