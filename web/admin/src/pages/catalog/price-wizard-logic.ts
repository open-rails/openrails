// Pure logic for the price-change wizard, kept apart from React for unit tests.
import { formatNativeAmount } from "@/lib/format"

export type PriceDirection = "increase" | "decrease" | "unchanged"

export function detectDirection(
  newAmount: string,
  currentAmount: string
): PriceDirection {
  const next = BigInt(newAmount)
  const current = BigInt(currentAmount)
  if (next > current) return "increase"
  if (next < current) return "decrease"
  return "unchanged"
}

export type MigrationMode = "grandfather" | "migrate"

// DEFAULT_NOTICE_WINDOW_DAYS is the fallback until the merchant's
// reprice_notice_window_days loads; it mirrors
// subscriptions.DefaultPriceIncreaseNoticeDays so the UI never under-gates.
// The server enforces the window too (price_increase_notice_too_short).
export const DEFAULT_NOTICE_WINDOW_DAYS = 30

// defaultMigrationMode is the direction-aware Step 2 default: an increase
// grandfathers (no new action), a decrease migrates now (never grandfathered).
export function defaultMigrationMode(direction: PriceDirection): MigrationMode {
  return direction === "decrease" ? "migrate" : "grandfather"
}

// minEffectiveDate is the earliest allowed migration date, or null when there
// is no minimum (a decrease needs no notice).
export function minEffectiveDate(
  direction: PriceDirection,
  now: Date,
  noticeWindowDays: number
): Date | null {
  if (direction !== "increase") return null
  const min = new Date(now)
  min.setUTCDate(min.getUTCDate() + noticeWindowDays)
  return min
}

// defaultEffectiveDate is Step 2's pre-filled migration date: the notice
// window's floor for an increase, "now" for a decrease.
export function defaultEffectiveDate(
  direction: PriceDirection,
  now: Date,
  noticeWindowDays: number
): Date {
  return minEffectiveDate(direction, now, noticeWindowDays) ?? now
}

// isEffectiveDateValid enforces the console-side notice-window gate: an
// increase+migrate plan may not pick a date inside the notice window.
export function isEffectiveDateValid(
  direction: PriceDirection,
  effectiveAt: Date,
  now: Date,
  noticeWindowDays: number
): boolean {
  const min = minEffectiveDate(direction, now, noticeWindowDays)
  return !min || effectiveAt.getTime() >= min.getTime()
}

export interface MigrationPlan {
  mode: MigrationMode
  // ISO instant; only meaningful when mode === "migrate".
  effectiveAt: string
}

const dateLabel = (iso: string) =>
  new Date(iso).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  })

// buildReviewText renders Step 3's plan in words, e.g. "New subscribers pay $12
// immediately. 1,204 existing subscribers keep $10 until Sep 1, then move to
// $12 at their next renewal. Notices go out on confirm."
export function buildReviewText(params: {
  newAmount: string
  currentAmount: string
  currency: string
  affectedCount: number
  plan: MigrationPlan
  now: Date
}): string {
  const { newAmount, currentAmount, currency, affectedCount, plan, now } =
    params
  const newLabel = formatNativeAmount(newAmount, currency)
  const oldLabel = formatNativeAmount(currentAmount, currency)
  const lead = `New subscribers pay ${newLabel} immediately.`

  if (affectedCount === 0) {
    return `${lead} No existing subscribers are on a prior version of this price.`
  }
  const subj = `${affectedCount.toLocaleString()} existing subscriber${affectedCount === 1 ? "" : "s"}`

  if (plan.mode === "grandfather") {
    return `${lead} ${subj} keep ${oldLabel} forever (grandfathered).`
  }

  // The engine notifies each affected subscriber at schedule time, however
  // far out effective_at is: always say so.
  const effective = new Date(plan.effectiveAt)
  const immediate = effective.getTime() <= now.getTime()
  if (immediate) {
    return `${lead} ${subj} move to ${newLabel} at their next renewal. Notices go out on confirm.`
  }
  return `${lead} ${subj} keep ${oldLabel} until ${dateLabel(plan.effectiveAt)}, then move to ${newLabel} at their next renewal. Notices go out on confirm.`
}
