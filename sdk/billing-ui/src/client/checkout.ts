// The purchase calls billing-ui's own components make: an app buys through
// <BuyButton> and <Offers>, never with these, so they are not exported. With
// orders (#1168) BuyButton creates an order instead, and apps don't change.
import type { CheckoutSource } from "../source"
import type { CheckoutSessionLink } from "./types"

export interface CheckoutSourceOptions {
  /**
   * A customer surface's base: its `/me` prefix, such as `/billing/v1/me` or
   * a host's own `/api/v1/merchants/acme/billing/me`. The session is read and
   * paid there with this client's credential, which proves the session's
   * customer, so its saved cards pay. Default: the session id alone, at
   * `{baseUrl}/checkout-sessions`.
   */
  customerBase?: string
}

export interface CheckoutCalls {
  createCheckoutSession(input: {
    priceKey?: string
    productKey?: string
    priceId?: string
    /** Customer-selected deposit in the price currency's native units. */
    amount?: string
    /** Defaults to true for recurring prices; false buys the initial term only. */
    autoRenew?: boolean
    successUrl?: string
  }): Promise<CheckoutSessionLink>
  checkoutSource(sessionId: string, opts?: CheckoutSourceOptions): CheckoutSource
}

const calls = new WeakMap<object, CheckoutCalls>()

export function registerCheckout(client: object, checkout: CheckoutCalls) {
  calls.set(client, checkout)
}

/** The checkout calls of a client from createBillingClient. */
export function checkoutOf(client: object): CheckoutCalls {
  const checkout = calls.get(client)
  if (!checkout)
    throw new Error("billing-ui: the client must come from createBillingClient")
  return checkout
}
