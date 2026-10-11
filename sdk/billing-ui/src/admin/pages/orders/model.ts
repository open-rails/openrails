import type { Order, Payment } from "#orck/client/generated/wire"
import type { UsageAction } from "#orck/admin/lib/api/order-endpoints"
import { amountFromInput } from "#orck/admin/lib/format"

export const usageActionLabels: Record<UsageAction, string> = {
  void: "Void order",
  mark_uncollectible: "Mark uncollectible",
  record_payment: "Record payment",
  retry_collection: "Retry collection",
}
export const usageActionDescriptions: Record<UsageAction, string> = {
  void: "Write off what the order still owes. Its payments stay recorded.",
  mark_uncollectible:
    "Stop scheduled collection. What the order owes remains owed; collecting it again reopens it.",
  record_payment:
    "Record money already received outside automatic collection. This does not charge a payment method.",
  retry_collection:
    "Charge what the order owes to the selected saved card now. An in-progress collection must finish first.",
}
// usageActions are what staff who may change billing can do to a usage
// order in its state: none while a collection is in progress.
export function usageActions(order: Order): UsageAction[] {
  const usage = order.usage
  if (!usage || BigInt(usage.amount_due) <= 0n) return []
  switch (order.status) {
    case "open":
      return [
        ...(usage.collection_method === "charge_automatically"
          ? (["retry_collection"] as const)
          : []),
        "record_payment",
        "mark_uncollectible",
        "void",
      ]
    case "uncollectible":
      return [
        ...(usage.collection_method === "charge_automatically"
          ? (["retry_collection"] as const)
          : []),
        "record_payment",
        "void",
      ]
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
