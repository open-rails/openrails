import type { BillingClient } from "./client/client"
import { loadStripeFor } from "./lib/stripe"
import { canAuthenticatePayment, type PspConfig } from "./psp"

/**
 * Completes the provider challenge (3-D Secure) a pending payment operation
 * waits on. "not_required" when the provider has none outstanding; the
 * operation's own status remains the result.
 */
export async function authenticatePayment(
  client: BillingClient,
  operationId: string,
  psp: PspConfig
): Promise<"authenticated" | "not_required"> {
  if (!canAuthenticatePayment(psp)) return "not_required"
  const { client_secret: secret } =
    await client.getPaymentAuthentication(operationId)
  if (!secret) return "not_required"
  const stripe = await loadStripeFor(psp.config!.publishable_key!)
  const result = await stripe.confirmCardPayment(secret)
  if (result.error)
    throw new Error(result.error.message ?? "Authentication failed.")
  await client.confirmPaymentAuthentication(operationId)
  return "authenticated"
}
