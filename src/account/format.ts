import type {
  CardSummary,
  CurrencyScales,
  Payment,
  Subscription,
  SubscriptionPrice,
} from "../client/types"
import type { Translator } from "../i18n/messages"
import { formatAmount } from "../lib/money"
import { accessLabel, everyLabel, intervalHours } from "../lib/period"

export function formatDate(
  value: string | null | undefined,
  locale?: string
): string | null {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  try {
    return new Intl.DateTimeFormat(locale, { dateStyle: "medium" }).format(date)
  } catch {
    return date.toISOString().slice(0, 10)
  }
}

/** Exact money, or null while the currency's scale is unknown. */
export function formatMoney(
  amount: string | null | undefined,
  currency: string,
  scales: CurrencyScales,
  locale?: string
): string | null {
  if (!amount) return null
  const decimals = scales[currency.toUpperCase()]
  if (decimals === undefined) return null
  return formatAmount(amount, currency, decimals, locale)
}

/** "every 30 days", "30 days of access" or "one-time". */
export function intervalLabel(
  price: SubscriptionPrice | null | undefined,
  m: Translator
): string | null {
  if (!price) return null
  const hours = price.access_duration_hours
  if (price.auto_renew === false)
    return accessLabel(hours, m) ?? m.t("interval.once")
  return everyLabel(hours, m)
}

export function subscriptionName(s: Subscription, { t }: Translator): string {
  return (
    s.product?.display_name?.trim() ||
    s.price?.key?.trim() ||
    t("subscriptions.fallbackName")
  )
}

/** What a payment bought: the product's name, else its kind, plus its cadence. */
export function paymentItem(
  p: Payment,
  m: Translator
): { name: string; detail: string | null } {
  const hours = intervalHours(p.price?.recurring?.interval)
  const recurring =
    p.price?.type === "recurring" || hours !== null || !!p.subscription_id
  const name =
    p.product?.display_name?.trim() ||
    m.t(recurring ? "history.subscription" : "history.purchase")
  return { name, detail: recurring ? everyLabel(hours, m) : null }
}

export function brandName(brand: string | null | undefined, fallback: string) {
  const value = brand?.trim()
  if (!value) return fallback
  return value[0].toUpperCase() + value.slice(1).toLowerCase()
}

export function expiry(card: CardSummary | null | undefined): string | null {
  if (!card?.exp_month || !card.exp_year) return null
  return `${String(card.exp_month).padStart(2, "0")}/${String(card.exp_year).slice(-2)}`
}

export const LIVE_STATUSES = new Set(["active", "pending", "past_due"])

const future = (at: string | null | undefined) =>
  !!at && new Date(at).getTime() > Date.now()

/** Still grants access: live, or cancelled with the paid period running. */
export const isLive = (s: Subscription) =>
  LIVE_STATUSES.has(s.status) ||
  (s.status === "cancelled" &&
    (!!s.cancel_scheduled || !!s.resumable) &&
    future(s.current_period_ends_at))

/** Access ends at period end and nothing renews it. */
export const isEnding = (s: Subscription) =>
  isLive(s) && (!!s.cancel_scheduled || s.status === "cancelled")

export type StatusTone = "success" | "warning" | "destructive" | "neutral"

const TONE: Record<string, StatusTone> = {
  active: "success",
  succeeded: "success",
  paid: "success",
  default: "neutral",
  pending: "warning",
  past_due: "warning",
  cancel_scheduled: "warning",
  expiring_soon: "warning",
  needs_attention: "warning",
  partially_refunded: "neutral",
  failed: "destructive",
  expired: "destructive",
  cancelled: "neutral",
  canceled: "neutral",
  refunded: "neutral",
}

export const statusTone = (status: string): StatusTone =>
  TONE[status] ?? "neutral"

// The package ships no preflight, so account surfaces zero UA text margins.
export const RESET =
  "[&_h2]:m-0 [&_p]:m-0 [&_ul]:m-0 [&_ul]:list-none [&_ul]:p-0 [&_table]:border-spacing-0"
