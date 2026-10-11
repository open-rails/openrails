import type { SubscriptionFilters } from "@/lib/api/endpoints"

// Views are named filters relative to today, kept in the URL as ?view=.
export const subscriptionViews = {
  renewing_soon: { label: "Renewing soon", days: 7 },
  recently_canceled: { label: "Recently canceled", days: -30 },
} as const
type SubscriptionView = keyof typeof subscriptionViews

const utcDay = (days: number, now = new Date()) =>
  new Date(now.getTime() + days * 86_400_000).toISOString().slice(0, 10)

// viewFilters is a view's filters on the day it is opened (UTC dates, as the
// API takes them).
export function viewFilters(
  view: string,
  now = new Date()
): SubscriptionFilters | null {
  switch (view) {
    case "renewing_soon":
      return {
        status: "active",
        expires_before: utcDay(subscriptionViews.renewing_soon.days, now),
        sort: "current_period_ends_at",
      }
    case "recently_canceled":
      return {
        canceled_after: utcDay(subscriptionViews.recently_canceled.days, now),
        sort: "canceled_at",
      }
  }
  return null
}

export const isView = (v: string): v is SubscriptionView =>
  v in subscriptionViews
