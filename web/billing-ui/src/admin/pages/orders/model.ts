import type { Order, Payment } from "#orck/client/generated/wire"
import type { UsageAction } from "#orck/admin/lib/api/order-endpoints"
import { amountFromInput } from "#orck/admin/lib/format"

export const usageActionLabels: Record<UsageAction, string> = {
  waive_balance: "Waive balance",
  mark_uncollectible: "Mark uncollectible",
  record_payment: "Record payment",
}
export const usageActionDescriptions: Record<UsageAction, string> = {
  waive_balance:
    "Forgive the remaining debt. Existing payments stay recorded and are not refunded.",
  mark_uncollectible:
    "Stop scheduled collection. What the order owes remains owed; collecting it again reopens it.",
  record_payment:
    "Record money already received outside automatic collection. This does not charge a payment method.",
}
// usageActions are what staff who may change billing can do to a usage
// order in its state: none while a collection is in progress.
export function usageActions(order: Order): UsageAction[] {
  const usage = order.usage
  if (order.collection_owner !== "engine" || !usage || BigInt(usage.amount_due) <= 0n) return []
  switch (order.status) {
    case "open":
      return ["record_payment", "mark_uncollectible", "waive_balance"]
    case "uncollectible":
      return ["record_payment", "waive_balance"]
  }
  return []
}
export function usageResultMessage(result: Order | Payment) {
  if ("kind" in result) return "Payment recorded."
  if (result.status === "complete") return "Order paid."
  if (result.status === "processing")
    return "Collection is pending verification. No new collection should be started."
  return "Order updated."
}
export function usagePaymentAmount(
  input: string,
  amountDue: string,
  decimals: number
) {
  const amount = amountFromInput(input.trim(), decimals)
  if (amount === null)
    throw new Error(
      `Enter a positive amount with up to ${decimals} decimal places.`
    )
  if (BigInt(amount) <= 0n || BigInt(amount) > BigInt(amountDue))
    throw new Error(
      "Payment must be positive and no greater than what the order owes."
    )
  return amount
}
