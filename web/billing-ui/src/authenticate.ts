import type { BillingClient } from "./client/client"
import type { SubscriptionChange } from "./client/types"
import { loadStripeFor } from "./lib/stripe"
import { canAuthenticatePayment, type PspConfig } from "./psp"

/**
 * Completes the issuer's authentication (3-D Secure) a subscription change
 * waits on, then confirms the change and answers it. A change with no
 * `authenticate` next action is answered as it is.
 */
export async function authenticateChange(
  client: BillingClient,
  subscriptionId: string,
  change: SubscriptionChange,
  psp: PspConfig
): Promise<SubscriptionChange> {
  const next = change.next_action
  const secret = next?.payload?.client_secret
  if (
    change.status !== "requires_action" ||
    next?.type !== "authenticate" ||
    !secret ||
    !canAuthenticatePayment(psp)
  )
    return change
  const stripe = await loadStripeFor(psp.config!.publishable_key!)
  const answered = await stripe.handleNextAction({ clientSecret: secret })
  if (answered.error)
    throw new Error(answered.error.message ?? "Authentication failed.")
  return client.confirmSubscriptionChange(subscriptionId)
}
