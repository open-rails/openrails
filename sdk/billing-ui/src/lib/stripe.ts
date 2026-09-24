// Stripe.js for in-page card setup and payment authentication. Loaded on first
// use, so hosts without a Stripe PSP never fetch it.
import type { Stripe } from "@stripe/stripe-js"

const loaded = new Map<string, Promise<Stripe>>()

export function loadStripeFor(publishableKey: string): Promise<Stripe> {
  let stripe = loaded.get(publishableKey)
  if (!stripe) {
    stripe = import("@stripe/stripe-js")
      .then(({ loadStripe }) => loadStripe(publishableKey))
      .then((value) => {
        if (!value) throw new Error("Stripe could not be loaded.")
        return value
      })
    stripe.catch(() => loaded.delete(publishableKey))
    loaded.set(publishableKey, stripe)
  }
  return stripe
}
