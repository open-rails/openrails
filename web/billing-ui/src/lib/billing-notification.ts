export interface BillingNotificationItem {
  kind: string
  data?: unknown
}
export type BillingNotificationAction =
  | "update_payment_method"
  | "resubscribe"
  | "authenticate_payment"
  | "contact_support"
  | "manage_billing"
export type BillingNotificationActions = Partial<
  Record<BillingNotificationAction, string>
>

export function dataOf(item: BillingNotificationItem): Record<string, unknown> {
  return item.data && typeof item.data === "object" && !Array.isArray(item.data)
    ? (item.data as Record<string, unknown>)
    : {}
}

/** An item's kind; one posted before invoices were named says order_collection_stopped. */
export function kindOf(item: BillingNotificationItem): string {
  return item.kind === "order_collection_stopped"
    ? "invoice_collection_stopped"
    : item.kind
}

/** The selected billing lifecycle outcomes, rather than signup/cancel confirmations. */
export function isPaymentFailure(kind: string): boolean {
  return [
    "payment_method_failed",
    "payment_method_update_required",
    "payment_authentication_required",
    "invoice_collection_stopped",
    "order_collection_stopped",
  ].includes(kind)
}

export function isBillingNotification(item: BillingNotificationItem): boolean {
  return (
    isPaymentFailure(item.kind) ||
    item.kind === "premium_ended" ||
    (item.kind === "premium_renewed" && dataOf(item).downgrade_applied === true)
  )
}

/** The immutable event's guidance; it is not a claim that a debt remains open. */
export function billingNotificationAction(
  item: BillingNotificationItem
): BillingNotificationAction {
  const action =
    dataOf(item).recovery_action ??
    (item.kind === "payment_method_update_required"
      ? "update_payment_method"
      : item.kind === "payment_authentication_required"
        ? "authenticate_payment"
        : undefined)
  return action === "update_payment_method" ||
    action === "resubscribe" ||
    action === "authenticate_payment" ||
    action === "contact_support"
    ? action
    : "manage_billing"
}
