import type {
  CardSummary,
  CurrencyScales,
  Subscription,
  SubscriptionPrice,
} from "../client/types"
import type { Translator } from "../i18n/messages"
import { formatAmount } from "../lib/money"

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

export function intervalLabel(
  price: SubscriptionPrice | null | undefined,
  { t }: Translator
): string | null {
  if (!price) return null
  const hours = price.access_duration_hours
  if (!hours) return price.auto_renew === false ? t("interval.once") : null
  if (price.auto_renew === false) return t("interval.once")
  if (hours % 24 !== 0) return t("interval.hours", { count: hours })
  const days = hours / 24
  if (days === 1) return t("interval.day")
  if (days === 7) return t("interval.week")
  if (days >= 28 && days <= 31) return t("interval.month")
  if (days >= 365 && days <= 366) return t("interval.year")
  return t("interval.days", { count: days })
}

export function subscriptionName(s: Subscription, { t }: Translator): string {
  return (
    s.product?.display_name?.trim() ||
    s.price?.key?.trim() ||
    t("subscriptions.fallbackName")
  )
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
