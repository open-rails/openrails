import type {
  CardSummary,
  CurrencyScales,
  Order,
  Payment,
  Price,
  Subscription,
} from "../client/types"
import type { Translator } from "../i18n/messages"
import { formatAmount } from "../lib/money"
import { accessLabel, everyLabel } from "../lib/period"

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
  price:
    | Pick<Price, "access_duration_hours" | "billing_interval_hours">
    | null
    | undefined,
  m: Translator
): string | null {
  if (!price) return null
  if (price.billing_interval_hours)
    return everyLabel(price.billing_interval_hours, m)
  return accessLabel(price.access_duration_hours, m) ?? m.t("interval.once")
}

export function subscriptionName(s: Subscription, { t }: Translator): string {
  return (
    s.product?.display_name?.trim() ||
    s.price?.key?.trim() ||
    t("subscriptions.fallbackName")
  )
}

/**
 * What a payment bought, from the frozen lines of the order it settles: their
 * names and, for one recurring line, its cadence. Without its order, the kind.
 */
export function paymentItem(
  p: Payment,
  order: Order | undefined,
  m: Translator
): { name: string; detail: string | null } {
  const names = [
    ...new Set(
      (order?.lines ?? []).map((l) => l.description.trim()).filter(Boolean)
    ),
  ]
  const line = order?.lines.length === 1 ? order.lines[0] : undefined
  return {
    name:
      names.join(", ") ||
      m.t(p.subscription_id ? "history.subscription" : "history.purchase"),
    detail: line?.billing_interval_hours
      ? everyLabel(line.billing_interval_hours, m)
      : null,
  }
}

export function brandName(brand: string | null | undefined, fallback: string) {
  const value = brand?.trim()
  if (!value) return fallback
  return value[0].toUpperCase() + value.slice(1).toLowerCase()
}

/** "Visa •••• 4242 · 12/30"; the brand alone when no digits are known. */
export function cardText(
  card: CardSummary | null | undefined,
  m: Translator
): string {
  const brand = brandName(card?.brand, m.t("paymentMethods.fallbackBrand"))
  if (!card?.last4) return brand
  const label = m.t("paymentMethods.cardLabel", { brand, last4: card.last4 })
  const expires = expiry(card)
  return expires ? `${label} · ${expires}` : label
}

export function expiry(card: CardSummary | null | undefined): string | null {
  if (!card?.exp_month || !card.exp_year) return null
  return `${String(card.exp_month).padStart(2, "0")}/${String(card.exp_year).slice(-2)}`
}

export const LIVE_STATUSES = new Set([
  "active",
  "pending",
  "past_due",
  "awaiting_method",
  "unverified",
])

const future = (at: string | null | undefined) =>
  !!at && new Date(at).getTime() > Date.now()

/** A current subscription, or a canceled subscription retaining paid access. */
export const isLive = (s: Subscription) =>
  LIVE_STATUSES.has(s.status) ||
  (s.status === "canceled" &&
    !!s.access &&
    (!s.access.ends_at || future(s.access.ends_at)))

/** Future billing has stopped or cancellation is scheduled. */
export const isEnding = (s: Subscription) =>
  isLive(s) && (!!s.cancel_scheduled || s.status === "canceled")

/** A subscription OpenRails could move to another plan of its tier group. */
export const changeable = (s: Subscription) =>
  (s.status === "active" || s.status === "past_due") &&
  !s.cancel_scheduled &&
  !s.cancel_portal_url &&
  !!s.product?.tier_group

export type StatusTone = "success" | "warning" | "destructive" | "neutral"

const TONE: Record<string, StatusTone> = {
  active: "success",
  succeeded: "success",
  paid: "success",
  default: "neutral",
  pending: "warning",
  past_due: "warning",
  awaiting_method: "warning",
  cancel_scheduled: "warning",
  expiring_soon: "warning",
  needs_attention: "warning",
  partially_refunded: "neutral",
  failed: "destructive",
  expired: "destructive",
  canceled: "neutral",
  refunded: "neutral",
}

export const statusTone = (status: string): StatusTone =>
  TONE[status] ?? "neutral"

// The package ships no preflight, so account surfaces zero UA text margins.
export const RESET =
  "[&_h2]:m-0 [&_p]:m-0 [&_ul]:m-0 [&_ul]:list-none [&_ul]:p-0 [&_table]:border-spacing-0"
